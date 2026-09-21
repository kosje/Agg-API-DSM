// Package pacer 把「RPM 硬墙」变成「平滑节拍」。
//
// 这是两个上游共享的核心能力之一 —— 迁移自 agnes-hub-go 的 internal/pacer。
// Copilot 同样有配额约束，同样会 429，所以它不该是 Agnes 的私产。
//
// # 为什么不用令牌桶 / 固定窗口
//
//   - 令牌桶允许突发，而突发恰好最容易命中上游「短时间内重复请求」的判定；
//   - 固定窗口在窗口边界会「双倍穿透」（第 59 秒与第 61 秒各打满一轮）。
//
// 实测佐证（Agnes 中国站）：22 RPM（间隔 2.73s）7/7 通过，而 30 RPM（间隔 2.0s）
// 第 2 个请求即被限 —— 说明上游约束的是「相邻间隔」，不是「60 秒窗口内计数」。
// 因此采用 FIFO 严格节拍：相邻两次放行间隔 = 窗口 / 有效 RPM。
//
// 相比最初的 Python 基线有一处改进：**准入控制在领号之前完成**。
// 基线是「先领号、再等超时报错」，超时的请求白烧了一个 RPM 槽位；
// 这里 Reserve 在加锁区间内就判定「等得起吗」，等不起直接拒绝、不占用槽位。
package pacer

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQueueFull 排队人数超过上限。
var ErrQueueFull = errors.New("排队人数已达上限")

// ErrTooLong 预计等待超过允许上限（未占用槽位）。
var ErrTooLong = errors.New("预计等待超过上限")

// Pacer 是一个限流桶的 FIFO 节拍器。零值不可用，请用 New。
type Pacer struct {
	mu         sync.Mutex
	rpm        float64
	windowSec  float64
	interval   time.Duration
	nextFreeAt time.Time
	waiting    int
}

// New 创建节拍器。rpm<=0 视为「不限制」（间隔为 0）。
func New(rpm, windowSec float64) *Pacer {
	if windowSec <= 0 {
		windowSec = 60
	}
	p := &Pacer{windowSec: windowSec}
	p.rpm = rpm
	p.interval = intervalFor(rpm, windowSec)
	return p
}

func intervalFor(rpm, windowSec float64) time.Duration {
	if rpm <= 0 {
		return 0
	}
	sec := windowSec / rpm
	return time.Duration(sec * float64(time.Second))
}

// Reconfigure 运行期调整有效 RPM。
//
// 重算间隔的同时把已承诺的 nextFreeAt 夹紧到「现在 + 新间隔 × 当前排队人数」，
// 避免下调 RPM 后已排队的请求仍要长时间背旧账
// （否则「校准生效」看起来要等几分钟，用户会以为没生效）。
func (p *Pacer) Reconfigure(rpm, windowSec float64) {
	if windowSec <= 0 {
		windowSec = 60
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rpm == rpm && p.windowSec == windowSec {
		return
	}
	p.rpm = rpm
	p.windowSec = windowSec
	p.interval = intervalFor(rpm, windowSec)
	if p.interval <= 0 {
		return
	}
	ceiling := time.Now().Add(p.interval * time.Duration(p.waiting))
	if p.nextFreeAt.After(ceiling) {
		p.nextFreeAt = ceiling
	}
}

// RPM 当前有效 RPM。
func (p *Pacer) RPM() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rpm
}

// Waiting 当前排队人数。
func (p *Pacer) Waiting() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiting
}

// ProjectedWait 如果此刻发起请求，预计要等多久。
//
// 账号选择靠它比较负载：把请求发给「等得最短」的账号，
// 而不是简单轮询 —— 后者会让某个慢账号拖住整批请求。
func (p *Pacer) ProjectedWait() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.nextFreeAt.After(now) {
		return p.nextFreeAt.Sub(now)
	}
	return 0
}

// Reserve 领一个发送槽位，返回排队等待时长。
//
// maxWait <= 0 表示不设等待上限；maxSize <= 0 表示不限制排队人数。
// 两种拒绝都在加锁区间内完成，因此**不会留下已占用但未使用的槽位**。
func (p *Pacer) Reserve(ctx context.Context, maxWait time.Duration, maxSize int) (time.Duration, error) {
	p.mu.Lock()
	if maxSize > 0 && p.waiting >= maxSize {
		p.mu.Unlock()
		return 0, ErrQueueFull
	}
	now := time.Now()
	start := now
	if p.nextFreeAt.After(now) {
		start = p.nextFreeAt
	}
	wait := start.Sub(now)
	if maxWait > 0 && wait > maxWait {
		p.mu.Unlock()
		return wait, ErrTooLong
	}
	p.nextFreeAt = start.Add(p.interval)
	p.waiting++
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.waiting > 0 {
			p.waiting--
		}
		p.mu.Unlock()
	}()

	if wait <= 0 {
		return 0, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return wait, nil
	case <-ctx.Done():
		return wait, ctx.Err()
	}
}

// Reset 清空节拍状态（控制台调参后强制立刻重新起算）。
func (p *Pacer) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextFreeAt = time.Time{}
}
