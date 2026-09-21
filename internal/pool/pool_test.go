package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"aggapi/internal/config"
	"aggapi/internal/pacer"
)

// newTestPacer 造一个不节流的节拍器。
//
// rpm 给 0：测试关心的是「谁被选中」「冷却对不对」，
// 不该被真实的限流等待拖慢。
func newTestPacer() *pacer.Pacer { return pacer.New(0, 60) }

// newTestPool 造一个带 n 个账号的池。
//
// 直接构造 account 而不是走 config.Store：Store 要落盘、要校验，
// 而这里要测的是调度逻辑本身，不该被存储层的细节牵住。
func newTestPool(t *testing.T, ids ...string) *Pool {
	t.Helper()
	p := New("test")
	p.mu.Lock()
	for _, id := range ids {
		p.accounts = append(p.accounts, &account{
			cfg:   config.Account{ID: id, Name: id, Enabled: true},
			pacer: newTestPacer(),
		})
		p.byID[id] = p.accounts[len(p.accounts)-1]
	}
	p.mu.Unlock()
	return p
}

// TestAcquireSkipsCoolingAccount 冷却中的账号不该被选中。
//
// 这是熔断的基本语义：失败过的账号在冷却期内必须被跳过，
// 否则「冷却」只是记了个时间戳，调度完全不受影响。
func TestAcquireSkipsCoolingAccount(t *testing.T) {
	p := newTestPool(t, "a", "b")

	// 让 a 失败一次，进入冷却。
	lease, err := p.Acquire(context.Background(), "", time.Second, 1)
	if err != nil {
		t.Fatalf("首次 Acquire 失败：%v", err)
	}
	failed := lease.AccountID()
	lease.Release(errors.New("boom"))

	// 接下来应该只能拿到另一个。
	next, err := p.Acquire(context.Background(), "", time.Second, 1)
	if err != nil {
		t.Fatalf("第二次 Acquire 失败：%v", err)
	}
	defer next.Release(nil)
	if next.AccountID() == failed {
		t.Fatalf("冷却中的账号 %s 仍被选中", failed)
	}
}

// TestCooldownDiffersByErrorType 不同错误类型给不同冷却时长。
//
// 这是刻意设计的差异：限流是「你太快了」等一会儿就好；
// 鉴权失效不会自己好，必须给长冷却，否则它会反复被选中、反复失败。
func TestCooldownDiffersByErrorType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"普通失败", errors.New("boom"), DefaultCooldown},
		{"被限流", RateLimited(errors.New("429")), DefaultRateLimitCooldown},
		{"鉴权失效", AuthFailed(errors.New("401")), DefaultAuthCooldown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPool(t, "a")
			lease, err := p.Acquire(context.Background(), "", time.Second, 1)
			if err != nil {
				t.Fatalf("Acquire 失败：%v", err)
			}
			lease.Release(c.err)

			st := p.Stats()[0]
			if st.Healthy {
				t.Fatal("失败后账号仍被判定为健康")
			}
			// 允许一点执行耗时带来的偏差。
			if st.Cooldown > c.want || st.Cooldown < c.want-time.Second {
				t.Fatalf("冷却时长 %v，期望约 %v", st.Cooldown, c.want)
			}
		})
	}
}

// TestStatsCountByErrorType 用量统计要按错误类型分开计。
//
// 控制台靠这几个数字回答「这个账号是被限流了还是凭据坏了」，
// 混在一起就没有诊断价值。
func TestStatsCountByErrorType(t *testing.T) {
	p := newTestPool(t, "a")

	for _, err := range []error{
		nil,
		errors.New("boom"),
		RateLimited(errors.New("429")),
		AuthFailed(errors.New("401")),
	} {
		lease, aerr := p.Acquire(context.Background(), "", time.Second, 1)
		if aerr != nil {
			t.Fatalf("Acquire 失败：%v", aerr)
		}
		lease.Release(err)
		// 每次失败都会让账号进入冷却，手动复活以便继续。
		p.Revive("a")
	}

	st := p.Stats()[0].Stats
	if st.Requests != 4 {
		t.Errorf("Requests = %d，期望 4", st.Requests)
	}
	if st.Errors != 3 {
		t.Errorf("Errors = %d，期望 3", st.Errors)
	}
	if st.RateLimited != 1 {
		t.Errorf("RateLimited = %d，期望 1", st.RateLimited)
	}
	if st.AuthFailed != 1 {
		t.Errorf("AuthFailed = %d，期望 1", st.AuthFailed)
	}
}

// TestRecoverNeedsConsecutiveSuccesses 恢复要连续成功，不是成功一次就清零。
//
// 一次成功可能是运气。strikes 归零意味着熔断彻底解除，
// 判早了会让坏账号重新进入轮换。
func TestRecoverNeedsConsecutiveSuccesses(t *testing.T) {
	p := newTestPool(t, "a")

	// 直接设定 strikes，避免依赖 Revive 的语义（它会一并清零）。
	p.mu.Lock()
	p.byID["a"].strikes = 3
	p.byID["a"].penaltyUntil = time.Time{}
	p.mu.Unlock()

	// 成功一次：strikes 应仍然存在（还没到连续成功阈值）。
	lease, _ := p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(nil)
	if got := p.Stats()[0].Strikes; got != 3 {
		t.Fatalf("一次成功后 strikes = %d，期望仍为 3", got)
	}

	// 再成功一次：达到阈值，清零。
	lease, _ = p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(nil)
	if got := p.Stats()[0].Strikes; got != 0 {
		t.Fatalf("连续成功后 strikes = %d，期望 0", got)
	}
}

// TestFailureResetsStreak 中途失败要打断「连续成功」的计数。
//
// 否则「成功、失败、成功」会被算成连续两次成功，把刚失败的账号
// 又判定成恢复了。
func TestFailureResetsStreak(t *testing.T) {
	p := newTestPool(t, "a")
	p.mu.Lock()
	p.byID["a"].strikes = 2
	p.mu.Unlock()

	lease, _ := p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(nil) // 成功 1 次，streak=1

	lease, _ = p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(errors.New("boom")) // 失败，streak 归零、strikes+1

	// 只清冷却、**保留 strikes 与 streak** —— 不能用 Revive：
	// 它的语义就是「把账号当全新的」，会把 strikes 一起清零，
	// 那样就测不出「失败是否打断了连续成功计数」了。
	p.mu.Lock()
	p.byID["a"].penaltyUntil = time.Time{}
	p.mu.Unlock()

	lease, _ = p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(nil) // 成功 1 次，streak=1（不是 2）

	if got := p.Stats()[0].Strikes; got == 0 {
		t.Fatal("strikes 被错误地清零了 —— 中间那次失败没打断连续计数")
	}
}

// TestAcquireExcludes 重试时要能排除指定账号。
//
// 这是「账号轮询」生效的关键：失败后必须换一个，
// 否则重试只是把同一个失败重复一遍。
func TestAcquireExcludes(t *testing.T) {
	p := newTestPool(t, "a", "b")

	lease, err := p.Acquire(context.Background(), "", time.Second, 1, "a")
	if err != nil {
		t.Fatalf("Acquire 失败：%v", err)
	}
	defer lease.Release(nil)
	if lease.AccountID() != "b" {
		t.Fatalf("排除 a 后拿到了 %s，期望 b", lease.AccountID())
	}

	// 全排除 -> 没有可用账号。
	if _, err := p.Acquire(context.Background(), "", time.Second, 1, "a", "b"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("全排除时应返回 ErrNoAccount，实际 %v", err)
	}
}

// TestMaxConcurrency 单账号并发上限要生效。
//
// 上游会把同账号的并发请求判定为异常，而且失败时会一起失败。
func TestMaxConcurrency(t *testing.T) {
	p := newTestPool(t, "a")
	p.maxConcurrency = 2

	l1, err := p.Acquire(context.Background(), "", time.Second, 1)
	if err != nil {
		t.Fatalf("第 1 个 Acquire 失败：%v", err)
	}
	l2, err := p.Acquire(context.Background(), "", time.Second, 1)
	if err != nil {
		t.Fatalf("第 2 个 Acquire 失败：%v", err)
	}
	// 第 3 个应该被并发上限挡住 —— 池里只有这一个账号，所以直接报无可用。
	if _, err := p.Acquire(context.Background(), "", time.Second, 1); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("超过并发上限时应返回 ErrNoAccount，实际 %v", err)
	}

	l1.Release(nil)
	// 归还后又能领到。
	l3, err := p.Acquire(context.Background(), "", time.Second, 1)
	if err != nil {
		t.Fatalf("归还后 Acquire 失败：%v", err)
	}
	l2.Release(nil)
	l3.Release(nil)
}

// TestReleaseIsIdempotent 重复释放不该把统计算两次。
//
// Release 可能在 defer 与显式调用两处都执行，重复计数会让控制台的
// 用量数字虚高，排查时被误导。
func TestReleaseIsIdempotent(t *testing.T) {
	p := newTestPool(t, "a")
	lease, _ := p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(nil)
	lease.Release(nil)
	lease.Release(nil)

	if got := p.Stats()[0].Stats.Requests; got != 1 {
		t.Fatalf("重复 Release 后 Requests = %d，期望 1", got)
	}
	if got := p.Stats()[0].InFlight; got != 0 {
		t.Fatalf("InFlight = %d，期望 0", got)
	}
}

// TestStickyOnlyAfterSuccess 只有成功的账号才被记成粘性。
//
// 把失败账号记成粘性，会让同一会话的下一次请求又优先选中它 ——
// 等于反复踩同一个坑。
func TestStickyOnlyAfterSuccess(t *testing.T) {
	p := newTestPool(t, "a", "b")

	// a 失败一次，不该被记住。
	lease, _ := p.Acquire(context.Background(), "sess", time.Second, 1, "b")
	failed := lease.AccountID()
	lease.Release(errors.New("boom"))

	p.mu.Lock()
	stuck := p.sticky["sess"]
	p.mu.Unlock()
	if stuck == failed {
		t.Fatalf("失败的账号 %s 被记成了粘性", failed)
	}

	// 成功一次，应该被记住。
	lease2, _ := p.Acquire(context.Background(), "sess", time.Second, 1)
	okID := lease2.AccountID()
	lease2.Release(nil)

	p.mu.Lock()
	stuck = p.sticky["sess"]
	p.mu.Unlock()
	if stuck != okID {
		t.Fatalf("成功后粘性 = %q，期望 %q", stuck, okID)
	}
}

// TestReviveClearsState 手动复活要清掉冷却与连续失败计数。
//
// 用户已经修好了问题（比如换了令牌），没必要干等冷却。
func TestReviveClearsState(t *testing.T) {
	p := newTestPool(t, "a")
	lease, _ := p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(errors.New("boom"))

	if !p.Revive("a") {
		t.Fatal("Revive 返回 false")
	}
	st := p.Stats()[0]
	if !st.Healthy || st.Strikes != 0 {
		t.Fatalf("复活后仍是 %+v", st)
	}
	if p.Revive("不存在") {
		t.Fatal("复活不存在的账号应返回 false")
	}
}

// TestResetStats 清统计只清用量，不动健康状态。
//
// 两个操作混在一起会出问题：用户想清个数字，结果把冷却也清了，
// 坏账号又被放回轮换。
func TestResetStats(t *testing.T) {
	p := newTestPool(t, "a")
	lease, _ := p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(errors.New("boom"))

	if !p.ResetStats("a") {
		t.Fatal("ResetStats 返回 false")
	}
	st := p.Stats()[0]
	if st.Stats.Requests != 0 || st.Stats.Errors != 0 {
		t.Fatalf("统计没清干净：%+v", st.Stats)
	}
	if st.Healthy {
		t.Fatal("清统计不应影响健康状态（冷却被一起清了）")
	}
}

// TestHealthyCount 健康计数要能反映实际可用数。
func TestHealthyCount(t *testing.T) {
	p := newTestPool(t, "a", "b", "c")
	if ok, total := p.Healthy(); ok != 3 || total != 3 {
		t.Fatalf("初始 %d/%d，期望 3/3", ok, total)
	}
	lease, _ := p.Acquire(context.Background(), "", time.Second, 1)
	lease.Release(errors.New("boom"))
	if ok, total := p.Healthy(); ok != 2 || total != 3 {
		t.Fatalf("一个失败后 %d/%d，期望 2/3", ok, total)
	}
}

// TestEmptyPool 没账号时要返回明确的错误。
//
// 上层靠 ErrNoAccount 决定返回 503；返回 nil 错误会让调用方
// 拿到一个空 Lease 然后 panic。
func TestEmptyPool(t *testing.T) {
	p := newTestPool(t)
	if _, err := p.Acquire(context.Background(), "", time.Second, 1); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("空池应返回 ErrNoAccount，实际 %v", err)
	}
}
