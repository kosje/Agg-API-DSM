// Package pool 是账号池：两个上游共用的调度核心。
//
// 这是本项目「互补而非重叠」的落点 —— 账号轮换、限流节拍、熔断冷却这套逻辑
// 从 Agnes 的业务里上提到这里，Copilot 上游同样受益（它也有配额、也会 429）。
//
// 熔断语义迁移自 agnes-hub-go 的 internal/hub：
//   - 失败 → 该账号进入冷却（penaltyUntil），冷却期内不再被选中
//   - 冷却到期自动复活（不需要人工介入，也不需要重启）
//
// 选择策略与上游略有不同：不按顺序轮询，而是**挑预计等待最短的账号**。
// 轮询会让一个慢账号拖住整批请求 —— 它的节拍器排着长队，却仍被轮到时发一次。
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
	// DefaultCooldown 是失败后的冷却时长。
	DefaultCooldown = 90 * time.Second
	// DefaultMaxQueue 是单账号允许的排队深度上限。
	// 超过说明这个账号已经严重落后，再往里塞只会让所有请求一起变慢。
	DefaultMaxQueue = 12
	// DefaultMaxWait 是单次请求允许的最长排队等待。
	DefaultMaxWait = 45 * time.Second
)

// account 是池内的单个账号状态。
type account struct {
	cfg config.Account

	pacer *pacer.Pacer

	// penaltyUntil 之前不再被选中。零值表示健康。
	penaltyUntil time.Time
	// strikes 是连续失败次数，用于日志与「是否已经打开熔断」的判断。
	strikes int
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
	// 软粘性：优先复用，但不强绑 —— 该账号不可用时立刻换。
	sticky map[string]string

	cooldown time.Duration
}

// New 构造账号池。
func New(providerName string) *Pool {
	return &Pool{
		providerName: providerName,
		byID:         map[string]*account{},
		sticky:       map[string]string{},
		cooldown:     DefaultCooldown,
	}
}

// Sync 用最新配置重建池。
//
// 保留既有账号的节拍器状态：配置里改了个名字不该让正在排队的请求重新起算，
// 也不该把熔断状态清掉（否则用户改一次配置就能「洗白」一个坏账号）。
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

	// 清掉已删除账号的状态，避免 byID 无限增长。
	for id := range p.byID {
		if !seen[id] {
			delete(p.byID, id)
		}
	}
	for _, a := range next {
		p.byID[a.cfg.ID] = a
	}
	p.accounts = next

	// 粘性表里指向已消失账号的条目一并清掉。
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
	// released 防止重复释放导致 strikes 被算两次。
	released bool
}

// Credential 返回账号，供 provider 填进请求。
func (l *Lease) Credential() any { return l.Account }

// Release 归还账号。err 为 nil 表示这次调用成功。
//
// 成功会清空连续失败计数并更新粘性；失败会累加计数并打开熔断。
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
// sessionKey 用于软粘性：同一个会话优先复用上次成功的账号
// （上游对同一账号的连续请求更友好，也更容易命中它的上下文缓存）。
// 传空字符串表示不需要粘性。
func (p *Pool) Acquire(ctx context.Context, sessionKey string,
	maxWait time.Duration, maxQueue int) (*Lease, error) {

	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}
	if maxQueue <= 0 {
		maxQueue = DefaultMaxQueue
	}

	p.mu.Lock()
	if len(p.accounts) == 0 {
		p.mu.Unlock()
		return nil, ErrNoAccount
	}

	now := time.Now()

	// 候选排序：预计等待短的优先。
	// 粘性账号给一个「减分」，让它排在等价负载的前面，但不强制。
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
		if !a.healthy(now) {
			continue
		}
		rank := 0
		if a.cfg.ID == stickyID {
			rank = -1 // 同等负载下优先复用
		}
		cands = append(cands, cand{a: a, wait: a.pacer.ProjectedWait(), rank: rank})
	}
	p.mu.Unlock()

	if len(cands) == 0 {
		// 全部在冷却中：报「没有可用账号」，让网关据此返回 503 并带上重试提示。
		return nil, ErrNoAccount
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].wait < cands[j].wait
	})

	// 依次尝试：某个账号排队过深时换下一个，而不是直接把请求拒掉。
	var lastErr error = ErrNoAccount
	for _, c := range cands {
		wait, err := c.a.pacer.Reserve(ctx, maxWait, maxQueue)
		if err != nil {
			lastErr = err
			continue
		}
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

// release 更新账号健康状态。
func (p *Pool) release(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	a, ok := p.byID[id]
	if !ok {
		return
	}
	if err == nil {
		a.strikes = 0
		a.penaltyUntil = time.Time{}
		return
	}
	a.strikes++
	a.penaltyUntil = time.Now().Add(p.cooldown)
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
	Cooldown time.Duration `json:"cooldown"`
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
			Cooldown: cd,
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
	a.pacer.Reset()
	return true
}

// Healthy 报告池内是否有可用账号。
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
