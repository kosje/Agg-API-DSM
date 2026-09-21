// Package pool 是账号池：两个上游共用的调度核心。
//
// 这是本项目「互补而非重叠」的落点 —— 账号轮换、限流节拍、熔断冷却这套逻辑
// 从 Agnes 的业务里上提到这里，Copilot 上游同样受益（它也有配额、也会 429）。
//
// 调度策略与两个原项目的差异：
//
//   - agnes-hub-go 按 FIFO 领号 + 校准系数选账号；
//   - M365 按冷却表 + 并发计数选账号。
//
// 这里取「**预计等待最短优先**」：它天然兼顾了限流节拍与负载，
// 不必再维护一套校准系数。粘性是软的 —— 优先复用上次成功的账号，
// 但它不可用时立刻换。
//
// 熔断语义迁移自 agnes-hub-go 的 internal/hub，并按 M365 的
// account_health 补上了「区分限流与鉴权失败」「单账号并发上限」「累计用量」。
package pool

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/pacer"
)

// ErrNoAccount 没有可用账号（未配置 / 全部冷却中 / 全部排队过深）。
var ErrNoAccount = errors.New("没有可用账号")

// Defaults 是池的默认参数。
const (
	// DefaultCooldown 是普通失败后的冷却时长。
	DefaultCooldown = 90 * time.Second
	// DefaultRateLimitCooldown 是被上游限流（429）后的冷却时长。
	//
	// 比普通失败短：限流是「你太快了」，等一会儿就好；
	// 而普通失败可能是账号本身坏了，需要更久来避开。
	DefaultRateLimitCooldown = 30 * time.Second
	// DefaultAuthCooldown 是鉴权失败后的冷却时长。
	//
	// 给得很长：凭据失效不会自己好，必须有人去重新授权。
	// 短冷却只会让它反复被选中、反复失败，把日志刷满。
	DefaultAuthCooldown = 30 * time.Minute

	// DefaultMaxQueue 是单账号允许的排队深度上限。
	DefaultMaxQueue = 12
	// DefaultMaxWait 是单次请求允许的最长排队等待。
	DefaultMaxWait = 45 * time.Second

	// DefaultMaxConcurrency 是单账号的并发上限。
	//
	// 上游会把同一账号的并发请求判定为异常，而且失败时会一起失败。
	// 给个保守值。
	DefaultMaxConcurrency = 4

	// DefaultRecoverSuccesses 是「连续成功几次才算真的恢复」。
	//
	// 一次成功不足以说明问题 —— 可能是运气。连续两次才把 strikes 清零。
	DefaultRecoverSuccesses = 2
)

// Stats 是单账号的累计用量。
//
// 字段对齐 agnes 的 AccountStats，让控制台能回答
// 「这个账号用得多不多、有没有在报错、被限流过几次」。
type Stats struct {
	Requests    int64     `json:"requests"`
	Errors      int64     `json:"errors"`
	RateLimited int64     `json:"rate_limited"`
	AuthFailed  int64     `json:"auth_failed"`
	LastError   string    `json:"last_error,omitempty"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
}

// account 是池内的单个账号状态。
type account struct {
	cfg config.Account

	pacer *pacer.Pacer

	// penaltyUntil 之前不再被选中。零值表示健康。
	penaltyUntil time.Time
	// strikes 是连续失败次数。
	strikes int
	// streak 是熔断后连续成功的次数，达到阈值才清零 strikes。
	streak int

	// inflight 是当前正在进行的请求数。
	inflight int

	// stats 是累计用量，只增不减（除非手动重置）。
	stats Stats
}

// healthy 判断账号当前是否可被选中。
func (a *account) healthy(now time.Time) bool {
	return !now.Before(a.penaltyUntil)
}

// Pool 管理某个上游的账号集合。
type Pool struct {
	providerName string

	mu       sync.Mutex
	accounts []*account
	byID     map[string]*account
	// sticky 记录「某个会话键上次成功用的是哪个账号」。
	sticky map[string]string

	cooldown       time.Duration
	rateCooldown   time.Duration
	authCooldown   time.Duration
	maxConcurrency int
}

// New 构造账号池。
func New(providerName string) *Pool {
	return &Pool{
		providerName:   providerName,
		byID:           map[string]*account{},
		sticky:         map[string]string{},
		cooldown:       DefaultCooldown,
		rateCooldown:   DefaultRateLimitCooldown,
		authCooldown:   DefaultAuthCooldown,
		maxConcurrency: DefaultMaxConcurrency,
	}
}

// Sync 用最新配置重建池。
//
// 保留既有账号的节拍器与统计：改个名字不该让正在排队的请求重新起算，
// 不该把熔断状态清掉（否则用户改一次配置就能「洗白」一个坏账号），
// 也不该把累计用量清零（那是排查问题的依据）。
func (p *Pool) Sync(store *config.Store) {
	cfgAccounts := store.AccountsFor(p.providerName)

	p.mu.Lock()
	defer p.mu.Unlock()

	next := make([]*account, 0, len(cfgAccounts))
	seen := map[string]bool{}

	for _, ca := range cfgAccounts {
		seen[ca.ID] = true
		if old, ok := p.byID[ca.ID]; ok {
			old.cfg = ca
			old.pacer.Reconfigure(float64(store.RPMFor(ca)), 60)
			next = append(next, old)
			continue
		}
		next = append(next, &account{
			cfg:   ca,
			pacer: pacer.New(float64(store.RPMFor(ca)), 60),
		})
	}

	for id := range p.byID {
		if !seen[id] {
			delete(p.byID, id)
		}
	}
	for _, a := range next {
		p.byID[a.cfg.ID] = a
	}
	p.accounts = next

	for k, id := range p.sticky {
		if !seen[id] {
			delete(p.sticky, k)
		}
	}
}

// Lease 是一次账号占用，用完必须 Release。
type Lease struct {
	Account config.Account
	pool    *Pool
	id      string
	// sessionKey 记录本次属于哪个会话，成功时据此更新粘性。
	sessionKey string
	// Wait 是实际排队等待时长，用于日志。
	Wait time.Duration
	// released 防止重复释放导致统计被算两次。
	released bool
}

// Credential 返回账号，供 provider 填进请求。
func (l *Lease) Credential() any { return l.Account }

// AccountID 返回本次占用的账号 ID，便于日志与重试判断。
func (l *Lease) AccountID() string { return l.id }

// Release 归还账号。err 为 nil 表示这次调用成功。
//
// 失败会按错误类型给不同的冷却时长 —— 这一点很关键：
//   - 被限流 → 短冷却，等一会儿就能再用
//   - 鉴权失效 → 长冷却，不人工介入不会自己好
//   - 其他 → 中等冷却
//
// 一刀切用同一个时长，要么让限流的账号闲置过久（浪费容量），
// 要么让坏账号反复被选中（刷满日志）。
func (l *Lease) Release(err error) {
	if l == nil || l.released {
		return
	}
	l.released = true
	l.pool.release(l.id, err)
	// 只有成功的账号才被记住：把失败账号记成粘性，
	// 会让同一会话的下一次请求又优先选中它，等于反复踩同一个坑。
	if err == nil {
		l.pool.MarkSticky(l.sessionKey, l.id)
	}
}

// Acquire 选一个账号并领到发送槽位。
//
// sessionKey 用于软粘性：同一个会话优先复用上次成功的账号。
// 传空字符串表示不需要粘性。
//
// exclude 里的账号会被跳过 —— 上层重试时用它避开刚失败的账号，
// 这是「轮询」真正生效的地方：失败后换一个，而不是原样重试。
func (p *Pool) Acquire(ctx context.Context, sessionKey string,
	maxWait time.Duration, maxQueue int, exclude ...string) (*Lease, error) {

	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}
	if maxQueue <= 0 {
		maxQueue = DefaultMaxQueue
	}
	skip := map[string]bool{}
	for _, id := range exclude {
		skip[id] = true
	}

	p.mu.Lock()
	if len(p.accounts) == 0 {
		p.mu.Unlock()
		return nil, ErrNoAccount
	}

	now := time.Now()
	stickyID := ""
	if sessionKey != "" {
		stickyID = p.sticky[sessionKey]
	}

	type cand struct {
		a    *account
		wait time.Duration
		rank int
	}
	var cands []cand
	for _, a := range p.accounts {
		if skip[a.cfg.ID] || !a.healthy(now) {
			continue
		}
		// 并发上限：已经在跑的请求太多时跳过这个账号。
		if p.maxConcurrency > 0 && a.inflight >= p.maxConcurrency {
			continue
		}
		rank := 0
		if a.cfg.ID == stickyID {
			rank = -1
		}
		cands = append(cands, cand{a: a, wait: a.pacer.ProjectedWait(), rank: rank})
	}
	p.mu.Unlock()

	if len(cands) == 0 {
		return nil, ErrNoAccount
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].wait < cands[j].wait
	})

	var lastErr error = ErrNoAccount
	for _, c := range cands {
		wait, err := c.a.pacer.Reserve(ctx, maxWait, maxQueue)
		if err != nil {
			lastErr = err
			continue
		}
		p.mu.Lock()
		c.a.inflight++
		p.mu.Unlock()
		return &Lease{
			Account:    c.a.cfg,
			pool:       p,
			id:         c.a.cfg.ID,
			sessionKey: sessionKey,
			Wait:       wait,
		}, nil
	}
	return nil, lastErr
}

// release 更新账号健康状态与统计。
func (p *Pool) release(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	a, ok := p.byID[id]
	if !ok {
		return
	}
	if a.inflight > 0 {
		a.inflight--
	}
	a.stats.Requests++
	a.stats.LastUsedAt = time.Now()

	if err == nil {
		// 连续成功到阈值才把 strikes 清零。
		// 一次成功不足以下结论 —— 可能是运气；而 strikes 归零意味着
		// 熔断彻底解除，判早了会让坏账号重新进入轮换。
		a.streak++
		if a.streak >= DefaultRecoverSuccesses {
			a.strikes = 0
			a.penaltyUntil = time.Time{}
		}
		return
	}

	a.streak = 0
	a.strikes++
	a.stats.Errors++
	a.stats.LastError = err.Error()

	cd := p.cooldown
	switch {
	case errors.Is(err, errRateLimited):
		a.stats.RateLimited++
		cd = p.rateCooldown
	case errors.Is(err, errAuthFailed):
		a.stats.AuthFailed++
		cd = p.authCooldown
	}
	a.penaltyUntil = time.Now().Add(cd)
}

// MarkSticky 记录「这个会话这次用的是哪个账号」。
//
// 只在请求真正成功后才调用 —— 失败的账号不该被记住。
func (p *Pool) MarkSticky(sessionKey, accountID string) {
	if sessionKey == "" || accountID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sticky[sessionKey] = accountID
}

// AccountStat 是给控制台看的单账号状态。
type AccountStat struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Healthy  bool          `json:"healthy"`
	Strikes  int           `json:"strikes"`
	RPM      float64       `json:"rpm"`
	Waiting  int           `json:"waiting"`
	InFlight int           `json:"in_flight"`
	Cooldown time.Duration `json:"cooldown"`
	Stats    Stats         `json:"stats"`
}

// Stats 返回池内所有账号的状态。
func (p *Pool) Stats() []AccountStat {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	out := make([]AccountStat, 0, len(p.accounts))
	for _, a := range p.accounts {
		cd := time.Duration(0)
		if now.Before(a.penaltyUntil) {
			cd = a.penaltyUntil.Sub(now)
		}
		out = append(out, AccountStat{
			ID:       a.cfg.ID,
			Name:     a.cfg.Name,
			Healthy:  a.healthy(now),
			Strikes:  a.strikes,
			RPM:      a.pacer.RPM(),
			Waiting:  a.pacer.Waiting(),
			InFlight: a.inflight,
			Cooldown: cd,
			Stats:    a.stats,
		})
	}
	return out
}

// Revive 手动复活一个正在冷却的账号（控制台用）。
func (p *Pool) Revive(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[id]
	if !ok {
		return false
	}
	a.penaltyUntil = time.Time{}
	a.strikes = 0
	a.streak = 0
	a.pacer.Reset()
	return true
}

// ResetStats 清空一个账号的累计用量。
func (p *Pool) ResetStats(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[id]
	if !ok {
		return false
	}
	a.stats = Stats{}
	return true
}

// Healthy 报告池内可用账号数与总数。
func (p *Pool) Healthy() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	ok := 0
	for _, a := range p.accounts {
		if a.healthy(now) {
			ok++
		}
	}
	return ok, len(p.accounts)
}

// 供上层判定的哨兵错误。
//
// 池不 import provider（那会形成反向依赖），所以这里定义自己的标记，
// 由 provider 在调用 Release 之前把错误翻译过来。
var (
	errRateLimited = errors.New("rate limited")
	errAuthFailed  = errors.New("auth failed")
)

// RateLimited 给一个错误打上「被上游限流」的标记。
func RateLimited(err error) error { return &tagged{err, errRateLimited} }

// AuthFailed 给一个错误打上「凭据失效」的标记。
func AuthFailed(err error) error { return &tagged{err, errAuthFailed} }

type tagged struct {
	error
	tag error
}

func (t *tagged) Unwrap() []error { return []error{t.error, t.tag} }
