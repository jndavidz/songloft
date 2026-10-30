package resourcebudget

import (
	"context"
	"errors"
	"sync"
)

// S-05：Songloft 本进程全局资源预算（首版静态分区，不新增协调服务）。
//
// - 七类生产者各占静态分区：cache / realtime / seek / radio / fingerprint /
//   probe / hls；每分区独立「活跃 + 等待」上限；
// - 满载明确 ErrBudgetFull（handler 层映射 503），不无限排队；
//   当前播放与 stop 优先经 AcquirePriority 插队等待队列；
// - 「取消后线程/子进程真实退出才释放」是调用方契约：release 只在真实工作
//   （含子进程）退出后调用，预算层不因 ctx 取消而提前释放工作槽；
// - musicdl/WebGUI 由 K-03/K-05 独立限额，跨项目 runner 用 Report() 聚合两侧
//   配置，断言总和不超静态分区总预算。

var ErrBudgetFull = errors.New("resource budget: partition saturated (503)")

// PartitionLimits 一个分区的静态上限。
type PartitionLimits struct {
	MaxActive  int `json:"max_active"`
	MaxWaiting int `json:"max_waiting"`
}

// Budget 全局预算表（首版静态配置，调参以实测为准）。
type Budget struct {
	mu sync.Mutex
	p  map[string]*partition
	// Limits 是初始静态配置；Report 原样上报。
	Limits map[string]PartitionLimits `json:"limits"`
}

type partition struct {
	limits PartitionLimits
	active int
	// waiters 是 FIFO 等待队列；AcquirePriority 的等待者插队（prepend）。
	waiters []chan struct{}
}

// DefaultBudget 首版静态分区。realtime（实时播放转码）与 cache（预缓存转码）
// 分离，保证当前播放优先；stop 命令本身不占重资源分区。
func DefaultBudget() *Budget {
	limits := map[string]PartitionLimits{
		"cache":       {MaxActive: 1, MaxWaiting: 8},  // 预缓存转码（含 managed prepare）
		"realtime":    {MaxActive: 1, MaxWaiting: 4},  // 实时播放转码（当前播放优先）
		"seek":        {MaxActive: 1, MaxWaiting: 4},  // seek 流式转码
		"radio":       {MaxActive: 2, MaxWaiting: 4},  // 电台拉流/转码
		"fingerprint": {MaxActive: 1, MaxWaiting: 2},  // 音频指纹
		"probe":       {MaxActive: 2, MaxWaiting: 16}, // 元数据探测（扫描/导入高频）
		"hls":         {MaxActive: 1, MaxWaiting: 4},  // HLS 生产
	}
	b := &Budget{p: map[string]*partition{}, Limits: limits}
	for name, l := range limits {
		b.p[name] = &partition{limits: l}
	}
	return b
}

// Acquire 获取一个工作槽。返回的 release 必须在真实工作（含子进程）退出后调用；
// ctx 取消只退出等待，不制造 release 义务。等待队列满返回 ErrBudgetFull。
func (b *Budget) Acquire(ctx context.Context, name string) (release func(), err error) {
	return b.acquire(ctx, name, false)
}

// AcquirePriority 供当前播放/stop 路径使用：插队等待队列。
func (b *Budget) AcquirePriority(ctx context.Context, name string) (release func(), err error) {
	return b.acquire(ctx, name, true)
}

func (b *Budget) acquire(ctx context.Context, name string, priority bool) (func(), error) {
	b.mu.Lock()
	p, ok := b.p[name]
	if !ok {
		b.mu.Unlock()
		return nil, errors.New("resource budget: unknown partition " + name)
	}
	if p.active < p.limits.MaxActive {
		p.active++
		b.mu.Unlock()
		return func() { b.release(name) }, nil
	}
	if len(p.waiters) >= p.limits.MaxWaiting {
		b.mu.Unlock()
		return nil, ErrBudgetFull
	}
	ch := make(chan struct{})
	if priority {
		p.waiters = append([]chan struct{}{ch}, p.waiters...)
	} else {
		p.waiters = append(p.waiters, ch)
	}
	b.mu.Unlock()

	select {
	case <-ctx.Done():
		b.removeWaiter(name, p, ch)
		return nil, ctx.Err()
	case <-ch:
		return func() { b.release(name) }, nil
	}
}

// removeWaiter 把仍在队列中的 ch 摘除（ctx 取消退出等待）。
// 若摘除时发现已被 release 唤醒（槽已转交给自己），需把槽还回给后续等待者。
func (b *Budget) removeWaiter(name string, p *partition, ch chan struct{}) {
	b.mu.Lock()
	for i, w := range p.waiters {
		if w == ch {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			b.mu.Unlock()
			return
		}
	}
	b.mu.Unlock()
	// 不在队列 → 已被 release 唤醒并拿到槽：把槽释放给下一个等待者
	select {
	case <-ch:
		b.release(name)
	default:
	}
}

func (b *Budget) release(name string) {
	b.mu.Lock()
	p := b.p[name]
	p.active--
	var next chan struct{}
	if len(p.waiters) > 0 {
		next = p.waiters[0]
		p.waiters = p.waiters[1:]
		p.active++ // 槽直接转交给下一个等待者
	}
	b.mu.Unlock()
	if next != nil {
		close(next)
	}
}

// Report 上报当前配置与实时占用（runner 聚合与诊断用；低基数指标）。
func (b *Budget) Report() map[string]map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]map[string]int{}
	for name, p := range b.p {
		out[name] = map[string]int{
			"max_active": p.limits.MaxActive, "max_waiting": p.limits.MaxWaiting,
			"active": p.active, "waiting": len(p.waiters),
		}
	}
	return out
}

// defaultBudget 是进程级单例（静态分区预算本就该全进程唯一）。
var defaultBudget = DefaultBudget()

// ProcessBudget 返回进程级预算表（接线点：managed prepare/transcode 及后续各服务）。
func ProcessBudget() *Budget { return defaultBudget }
