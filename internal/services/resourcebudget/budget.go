package resourcebudget

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
)

// S-05：Songloft 本进程全局资源预算（静态分区配额 + 一条进程级总闸门）。
//
// - 七类生产者各占静态分区：cache / realtime / seek / radio / fingerprint /
//   probe / hls；每分区独立「活跃 + 等待」上限；
// - **总闸门**：各分区活跃之和另受 maxTotal 约束。分区配额本身不等于全局上限——
//   收编前各生产者持的是各自的私有信号量（落盘转码 1、seek+HLS+URL 共享 4、
//   元数据探测 2、指纹 GOMAXPROCS÷4、电台 2），叠起来在 4 核 NAS 上最坏可达十来个
//   ffmpeg 并发，那正是「CPU 打满 → 首字节卡顿」要消除的形态；
// - 后台分区（fingerprint / probe）带 Reserve：不得把总活跃推到 maxTotal-Reserve
//   以上，给当前播放/seek 留头寸，否则整库扫描会把交互路径饿死；
// - 满载明确 ErrBudgetFull（handler 层映射 503），不无限排队；
//   当前播放与 stop 优先经 AcquirePriority 插队等待队列，满了就降级的用 TryAcquire；
// - 「取消后线程/子进程真实退出才释放」是调用方契约：release 只在真实工作
//   （含子进程）退出后调用，预算层不因 ctx 取消而提前释放工作槽；
// - musicdl/WebGUI 由 K-03/K-05 独立限额，跨项目 runner 用 Report() 聚合两侧
//   配置，断言总和不超静态分区总预算。

var ErrBudgetFull = errors.New("resource budget: saturated (503)")

// envMaxTotal 允许不重新编译就按机器调参（弱 CPU 收紧、强机放开）。
const envMaxTotal = "SONGLOFT_MEDIA_MAX_TOTAL"

// backgroundReserve 是后台分区合计必须为交互路径留出的槽位数。
const backgroundReserve = 2

// minMaxTotal 是总闸门的下限：低于它，「播一首 + 转一首 + 后台补一首」就会互相掐。
const minMaxTotal = 6

// 分区名是预算表的词汇表，接线点必须用这些常量（散落字符串拼错不会编译报错）。
const (
	PartitionCache       = "cache"       // 落盘转码（含 managed prepare）
	PartitionRealtime    = "realtime"    // 实时播放转码（不入库直链）
	PartitionSeek        = "seek"        // seek/均衡/变速流 + 音频 HLS 直播（长存活 ffmpeg）
	PartitionRadio       = "radio"       // 电台拉流/转码（长存活 ffmpeg）
	PartitionFingerprint = "fingerprint" // 音频指纹（后台）
	PartitionProbe       = "probe"       // 元数据探测（后台）
	PartitionHLS         = "hls"         // HLS 段生产（视频 → HLS 后台转码）
)

// PartitionLimits 一个分区的静态上限。
type PartitionLimits struct {
	MaxActive  int `json:"max_active"`
	MaxWaiting int `json:"max_waiting"`
	// Reserve 非零时，该分区不得把进程总活跃推到 maxTotal-Reserve 以上
	// （后台生产者用；前台分区为 0）。
	Reserve int `json:"reserve"`
}

// Budget 全局预算表（静态分区配额 + 一条总闸门）。
type Budget struct {
	mu       sync.Mutex
	p        map[string]*partition
	maxTotal int
	total    int
	// Limits 是初始静态配置；Report 原样上报。
	Limits map[string]PartitionLimits `json:"limits"`
}

type partition struct {
	limits PartitionLimits
	active int
	// waiters 是 FIFO 等待队列；AcquirePriority 的等待者插队（prepend）。
	waiters []chan struct{}
}

// DefaultBudget 首版静态配置：分区上限与接线前各生产者的私有闸门一致，
// 避免「收编进预算」这件事本身造成并发回退。realtime（实时播放转码）与
// cache（预缓存转码）分离，保证当前播放优先；stop 命令本身不占重资源分区。
func DefaultBudget() *Budget {
	limits := map[string]PartitionLimits{
		PartitionCache:       {MaxActive: 1, MaxWaiting: 8},
		PartitionRealtime:    {MaxActive: 2, MaxWaiting: 4},
		PartitionSeek:        {MaxActive: 4, MaxWaiting: 4},
		PartitionRadio:       {MaxActive: 2, MaxWaiting: 4},
		PartitionFingerprint: {MaxActive: fingerprintSlots(), MaxWaiting: 4, Reserve: backgroundReserve},
		PartitionProbe:       {MaxActive: 2, MaxWaiting: 16, Reserve: backgroundReserve},
		PartitionHLS:         {MaxActive: 2, MaxWaiting: 4},
	}
	return New(limits, defaultMaxTotal())
}

// New 用给定配额与总闸门构造预算表（测试与非默认部署用）。
// maxTotal <= 0 表示不设总闸门（只按分区配额准入）。
func New(limits map[string]PartitionLimits, maxTotal int) *Budget {
	b := &Budget{p: map[string]*partition{}, Limits: limits, maxTotal: maxTotal}
	for name, l := range limits {
		b.p[name] = &partition{limits: l}
	}
	return b
}

// fingerprintSlots 指纹是后台低优先级任务，只取 GOMAXPROCS 的四分之一（下限 1、上限 4），
// 避免在 4 核 NAS 这类设备上把 CPU 打满（songloft-org/songloft#323）。
// Go 的 GOMAXPROCS 感知 cgroup CPU 限额，所以 Docker 限核也能正确收敛。
func fingerprintSlots() int {
	n := runtime.GOMAXPROCS(0) / 4
	if n < 1 {
		return 1
	}
	if n > 4 {
		return 4
	}
	return n
}

// defaultMaxTotal 是进程级重资源（ffmpeg/ffprobe）并发总上限。
//
// 取 CPU 数 + 2、下限 6：一条播放流 + 一次预缓存转码 + 少量交互 seek 不会被掐，
// 而失控的扇出（整库指纹、批量探测、多路 HLS）会在总闸门处明确 503，
// 而不是把 CPU 拖死成「没人能播」。
func defaultMaxTotal() int {
	n := runtime.GOMAXPROCS(0) + 2
	if raw := os.Getenv(envMaxTotal); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		} else {
			slog.Warn("resource budget: 忽略非法的总闸门配置", "value", raw, "used", n)
		}
	}
	if n < minMaxTotal {
		n = minMaxTotal
	}
	return n
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

// TryAcquire 非阻塞取槽：拿不到立刻 ErrBudgetFull，绝不排队。
//
// 给「满了就降级」的调用方用（seek / HLS / 远程 URL 转码——排队等于让音箱干等，
// 比 503 更糟），语义与收编前的私有信号量 `select { case sem <- {}: default: }` 一致。
func (b *Budget) TryAcquire(name string) (release func(), err error) {
	b.mu.Lock()
	p, ok := b.p[name]
	if !ok {
		b.mu.Unlock()
		return nil, errors.New("resource budget: unknown partition " + name)
	}
	granted := b.canAdmitLocked(p)
	if granted {
		b.grantLocked(p)
	}
	b.mu.Unlock()
	if !granted {
		return nil, ErrBudgetFull
	}
	return func() { b.release(name) }, nil
}

func (b *Budget) acquire(ctx context.Context, name string, priority bool) (func(), error) {
	b.mu.Lock()
	p, ok := b.p[name]
	if !ok {
		b.mu.Unlock()
		return nil, errors.New("resource budget: unknown partition " + name)
	}
	if b.canAdmitLocked(p) {
		b.grantLocked(p)
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

// canAdmitLocked 判定该分区现在能否再接一个活跃工作：分区配额、进程总闸门、
// 后台分区的 Reserve 头寸三者都过才放行。调用方必须持有 b.mu。
func (b *Budget) canAdmitLocked(p *partition) bool {
	if p.active >= p.limits.MaxActive {
		return false
	}
	if b.maxTotal > 0 && b.total >= b.maxTotal {
		return false
	}
	if p.limits.Reserve > 0 && b.maxTotal > 0 && b.total >= b.maxTotal-p.limits.Reserve {
		return false
	}
	return true
}

// grantLocked 记一次占用。调用方必须持有 b.mu。
func (b *Budget) grantLocked(p *partition) {
	p.active++
	b.total++
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
	b.total--
	ready := b.dispatchLocked()
	b.mu.Unlock()
	for _, ch := range ready {
		close(ch)
	}
}

// dispatchLocked 唤醒「现在已可准入」的等待者。
//
// 必须扫全表而不是只接续本分区队头：一个等待者可能不是因为分区满、而是因为总闸门满
// 才排队，解开它的槽来自**别的分区**的释放。按分区名排序遍历，使同一次释放里的授予
// 顺序确定（便于复现与测试）；分区内部仍是 FIFO，priority 靠插队。
func (b *Budget) dispatchLocked() []chan struct{} {
	names := make([]string, 0, len(b.p))
	for name := range b.p {
		names = append(names, name)
	}
	sort.Strings(names)

	var ready []chan struct{}
	for _, name := range names {
		p := b.p[name]
		for len(p.waiters) > 0 && b.canAdmitLocked(p) {
			next := p.waiters[0]
			p.waiters = p.waiters[1:]
			b.grantLocked(p)
			ready = append(ready, next)
		}
	}
	return ready
}

// Report 上报当前配置与实时占用（runner 聚合与诊断用；低基数指标）。
// 保留键 "_total" 给出进程级总闸门（max / active / 全部队列 waiting）。
func (b *Budget) Report() map[string]map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]map[string]int{}
	waiting := 0
	for name, p := range b.p {
		out[name] = map[string]int{
			"max_active": p.limits.MaxActive, "max_waiting": p.limits.MaxWaiting,
			"reserve": p.limits.Reserve, "active": p.active, "waiting": len(p.waiters),
		}
		waiting += len(p.waiters)
	}
	out["_total"] = map[string]int{
		"max": b.maxTotal, "active": b.total, "waiting": waiting,
	}
	return out
}

// defaultBudget 是进程级单例（静态分区预算本就该全进程唯一）。
var defaultBudget = DefaultBudget()

// ProcessBudget 返回进程级预算表（接线点：managed prepare/transcode 及后续各服务）。
func ProcessBudget() *Budget { return defaultBudget }
