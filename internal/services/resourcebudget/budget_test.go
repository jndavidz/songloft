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
