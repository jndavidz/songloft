package resourcebudget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBudget(maxActive, maxWaiting int) *Budget {
	b := DefaultBudget()
	b.p["test"] = &partition{limits: PartitionLimits{MaxActive: maxActive, MaxWaiting: maxWaiting}}
	return b
}

// TestBudget_Saturation503：活跃+等待全满 → ErrBudgetFull（handler 层映射 503）。
func TestBudget_Saturation503(t *testing.T) {
	b := newTestBudget(1, 1)
	ctx := context.Background()
	r1, _ := b.Acquire(ctx, "test")

	// 等待位由 goroutine 占据（Acquire 是阻塞语义）
	acquired := make(chan func(), 1)
	go func() {
		w, err := b.Acquire(ctx, "test")
		if err != nil {
			close(acquired)
			return
		}
		acquired <- w
	}()
	time.Sleep(10 * time.Millisecond) // 确保已入队

	// 活跃满 + 等待满 → 明确 503
	if _, err := b.Acquire(ctx, "test"); !errors.Is(err, ErrBudgetFull) {
		t.Fatalf("满载应 ErrBudgetFull，got %v", err)
	}
	// 释放后等待者获得槽
	r1()
	select {
	case w := <-acquired:
		w()
	case <-time.After(time.Second):
		t.Fatal("等待者未按时获得槽")
	}
}

// TestBudget_FIFOAndPriority：普通等待 FIFO；priority 插队。
func TestBudget_FIFOAndPriority(t *testing.T) {
	b := newTestBudget(1, 4)
	ctx := context.Background()
	r1, _ := b.Acquire(ctx, "test")

	order := make(chan string, 4)
	// 10ms 后 r1 释放；等待者 wN1（队列头）随即拿到槽——释放 goroutine 必须先于阻塞的 Acquire 启动
	go func() { time.Sleep(10 * time.Millisecond); r1() }()
	wN1, _ := b.Acquire(ctx, "test")
	go func() {
		order <- "normal1"
		wN1()
	}()
	// 槽在 normal1 手里；priority 插队（排在普通 normal2 前面）
	errCh := make(chan error, 2)
	go func() {
		rp, err := b.AcquirePriority(ctx, "test")
		if err != nil {
			errCh <- err
			return
		}
		order <- "priority"
		rp()
	}()
	time.Sleep(5 * time.Millisecond) // 确保 priority 先于 normal2 入队
	go func() {
		rn, err := b.Acquire(ctx, "test")
		if err != nil {
			errCh <- err
			return
		}
		order <- "normal2"
		rn()
	}()

	recv := func(what string) string {
		select {
		case v := <-order:
			t.Logf("%s 收到 %q", what, v)
			return v
		case err := <-errCh:
			t.Fatalf("%s: Acquire 失败: %v", what, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: 超时，budget=%v", what, b.Report()["test"])
		}
		return ""
	}
	// 3 个 send 对 3 个 recv：normal1 / priority / normal2
	got := []string{recv("1st"), recv("2nd"), recv("3rd")}
	if got[0] != "normal1" || got[1] != "priority" || got[2] != "normal2" {
		t.Errorf("priority 应插队: %v", got)
	}
}

// TestBudget_CancelDoesNotLeakSlot：ctx 取消退出等待，槽不泄漏、计数归零。
func TestBudget_CancelDoesNotLeakSlot(t *testing.T) {
	b := newTestBudget(1, 2)
	ctx := context.Background()
	r1, _ := b.Acquire(ctx, "test")

	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := b.Acquire(cctx, "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("应 context.Canceled，got %v", err)
	}
	r1()
	// 取消的等待者不得占用槽：现在应能满额拿满 1 个活跃槽
	r2, err := b.Acquire(ctx, "test")
	if err != nil {
		t.Fatalf("取消后应能拿到槽: %v", err)
	}
	rep := b.Report()["test"]
	if rep["active"] != 1 || rep["waiting"] != 0 {
		t.Errorf("计数应归位: %+v", rep)
	}
	r2()
	if rep := b.Report()["test"]; rep["active"] != 0 {
		t.Errorf("全释放后 active=0，got %d", rep["active"])
	}
}

// TestBudget_RealWorkHoldsSlot：release 只在真实工作退出后调用——
// 用真实 goroutine 生命周期验证（断言真实 worker 存活期，而非 future 取消）。
func TestBudget_RealWorkHoldsSlot(t *testing.T) {
	b := newTestBudget(1, 0)
	ctx := context.Background()
	rel, err := b.Acquire(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	var workerDone atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(80 * time.Millisecond) // 模拟真实工作（子进程运行中）
		rel()                             // 工作真实退出后才释放
		workerDone.Store(true)
	}()
	time.Sleep(30 * time.Millisecond) // 工作仍在跑
	if rep := b.Report()["test"]; rep["active"] != 1 {
		t.Errorf("真实工作期间槽必须被占住，active=%d", rep["active"])
	}
	<-done
	if !workerDone.Load() {
		t.Fatal("worker 应已完成")
	}
	if rep := b.Report()["test"]; rep["active"] != 0 {
		t.Errorf("工作退出后 active=0，got %d", rep["active"])
	}
}

// TestBudget_ConcurrentStress：并发压测无死锁、无计数漂移。
func TestBudget_ConcurrentStress(t *testing.T) {
	b := newTestBudget(2, 3)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			rel, err := b.Acquire(ctx, "test")
			if err != nil {
				return // 503/取消都是合法结局
			}
			time.Sleep(time.Millisecond)
			rel()
		}(i)
	}
	wg.Wait()
	if rep := b.Report()["test"]; rep["active"] != 0 {
		t.Errorf("全部退出后 active=0，got %d", rep["active"])
	}
}

// TestBudget_TotalCap：总闸门是跨分区的——各分区配额之和允许超过 maxTotal，
// 但同时活跃的和必须压在 maxTotal 之下（收编前生产者各守自家信号量，没有这条约束）。
func TestBudget_TotalCap(t *testing.T) {
	limits := map[string]PartitionLimits{
		PartitionCache: {MaxActive: 4, MaxWaiting: 0},
		PartitionSeek:  {MaxActive: 4, MaxWaiting: 4},
	}
	b := New(limits, 3)
	ctx := context.Background()

	var releases []func()
	for i := 0; i < 3; i++ {
		rel, err := b.Acquire(ctx, PartitionCache)
		if err != nil {
			t.Fatalf("总数未满时第 %d 个槽应获得: %v", i+1, err)
		}
		releases = append(releases, rel)
	}
	if got := b.Report()["_total"]["active"]; got != 3 {
		t.Fatalf("总活跃=%d want 3", got)
	}
	// 另一个分区配额仍空闲，但总数到顶 ⇒ 必须拒绝。
	if _, err := b.TryAcquire(PartitionSeek); !errors.Is(err, ErrBudgetFull) {
		t.Fatalf("总数已满时 TryAcquire 应拒绝，got %v", err)
	}

	// 阻塞等待者排在**总数**上限上：释放别的分区的槽也必须把它唤醒
	// （只接续本分区队列的实现会把它挂死）。
	granted := make(chan struct{})
	go func() {
		rel, err := b.Acquire(ctx, PartitionSeek)
		if err != nil {
			t.Errorf("等待 seek 槽失败: %v", err)
			return
		}
		rel()
		close(granted)
	}()
	time.Sleep(20 * time.Millisecond) // 让等待者入队
	releases[0]()                     // 释放 cache 槽 → 解开 seek 等待者
	select {
	case <-granted:
	case <-time.After(2 * time.Second):
		t.Fatalf("跨分区释放未唤醒等待者: %v", b.Report()["_total"])
	}
	for _, rel := range releases[1:] {
		rel()
	}
	if got := b.Report()["_total"]["active"]; got != 0 {
		t.Fatalf("全释放后总活跃=%d want 0", got)
	}
}

// TestBudget_BackgroundReserve：后台分区不得吃掉交互路径的头寸，
// 否则整库指纹重算会把播放/seek 饿死。
func TestBudget_BackgroundReserve(t *testing.T) {
	limits := map[string]PartitionLimits{
		PartitionFingerprint: {MaxActive: 4, MaxWaiting: 0, Reserve: 2},
		PartitionSeek:        {MaxActive: 4, MaxWaiting: 0},
	}
	b := New(limits, 4) // maxTotal=4、Reserve=2 ⇒ 后台最多占 2

	for i := 0; i < 2; i++ {
		if _, err := b.TryAcquire(PartitionFingerprint); err != nil {
			t.Fatalf("后台第 %d 个槽应获得: %v", i+1, err)
		}
	}
	if _, err := b.TryAcquire(PartitionFingerprint); !errors.Is(err, ErrBudgetFull) {
		t.Fatalf("后台越过了 Reserve 头寸，got %v", err)
	}
	// 留给交互路径的头寸必须真的可用。
	for i := 0; i < 2; i++ {
		if _, err := b.TryAcquire(PartitionSeek); err != nil {
			t.Fatalf("交互第 %d 个槽应获得: %v", i+1, err)
		}
	}
}

// TestBudget_TryAcquireFailsFast：TryAcquire 绝不排队（满了即拒绝），
// 且拒绝不改变任何计数——这是 seek/HLS 的「降级而非干等」契约。
func TestBudget_TryAcquireFailsFast(t *testing.T) {
	b := New(map[string]PartitionLimits{"test": {MaxActive: 1, MaxWaiting: 8}}, 6)
	rel, err := b.TryAcquire("test")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := b.TryAcquire("test"); !errors.Is(err, ErrBudgetFull) {
		t.Fatalf("第二个应拒绝，got %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("TryAcquire 阻塞了 %v（契约是不排队）", d)
	}
	if rep := b.Report()["test"]; rep["active"] != 1 || rep["waiting"] != 0 {
		t.Fatalf("被拒绝的请求改变了计数: %+v", rep)
	}
	rel()
	if _, err := b.TryAcquire("test"); err != nil {
		t.Fatalf("释放后应能复用: %v", err)
	}
}

// TestBudget_TotalCapStress：跨分区并发取放，进程总活跃**任何时刻**都不得超过 maxTotal。
// 这是「全局单闸门」的核心不变量（分区配额之和故意大于总数，靠的就是这条硬约束）。
func TestBudget_TotalCapStress(t *testing.T) {
	limits := map[string]PartitionLimits{
		PartitionCache:       {MaxActive: 4, MaxWaiting: 8},
		PartitionSeek:        {MaxActive: 4, MaxWaiting: 8},
		PartitionFingerprint: {MaxActive: 4, MaxWaiting: 8, Reserve: 2},
	}
	const maxTotal = 3
	b := New(limits, maxTotal)

	var wg sync.WaitGroup
	var peak atomic.Int64
	for i := 0; i < 60; i++ {
		name := []string{PartitionCache, PartitionSeek, PartitionFingerprint}[i%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			rel, err := b.Acquire(ctx, name)
			if err != nil {
				return // 503/超时都是合法结局
			}
			if got := int64(b.Report()["_total"]["active"]); got > peak.Load() {
				peak.Store(got)
			}
			time.Sleep(time.Millisecond)
			rel()
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > maxTotal {
		t.Fatalf("并发中出现总活跃 %d > maxTotal %d", got, maxTotal)
	}
	if rep := b.Report()["_total"]; rep["active"] != 0 {
		t.Fatalf("全部退出后总活跃=%d want 0", rep["active"])
	}
	t.Logf("并发峰值总活跃=%d（上限 %d）", peak.Load(), maxTotal)
}

// TestBudget_DefaultWiredPartitions：默认预算表里每个声明的分区都要有接线点，
// 且 Σ 配额 > maxTotal（否则总闸门形同虚设）。
func TestBudget_DefaultWiredPartitions(t *testing.T) {
	rep := DefaultBudget().Report()
	total := 0
	for _, name := range []string{
		PartitionCache, PartitionRealtime, PartitionSeek, PartitionRadio,
		PartitionFingerprint, PartitionProbe, PartitionHLS,
	} {
		p, ok := rep[name]
		if !ok {
			t.Fatalf("默认预算缺分区 %q", name)
		}
		if p["max_active"] <= 0 {
			t.Errorf("%s 配额异常: %d", name, p["max_active"])
		}
		total += p["max_active"]
	}
	maxTotal := rep["_total"]["max"]
	if total <= maxTotal {
		t.Errorf("Σ 分区配额=%d 未超过总闸门=%d：总闸门不会生效", total, maxTotal)
	}
}
