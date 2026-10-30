package services

// 受控故障注入：转码槽饱和（HLS 直播会话走进程资源预算的 seek 分区）。
//
// 契约（hls_transcode.go Create / resourcebudget 分区表）：
//  1. 每个 HLS 会话在生命周期内持有 1 个 seek 分区槽，容量 = 预算表 seek.max_active
//     （与收编前的包级 seekStreamSem 容量一致）；
//  2. 槽位满时 Create 必须 **立即** 返回明确错误（503 "转码并发已满"），
//     既不排队（无界等待）也不静默成功；
//  3. 任一存活会话结束（Delete / 失败 / 闲置回收）必须归还槽位，
//     后续 Create 可以重新成功——槽位不泄漏；
//  4. 全部结束后分区活跃与进程总活跃（_total）都归零：预算槽随真实工作退出而释放。
//
// 本测试用本地回环 httptest 提供 60s TS 夹具，不触碰任何真实部署。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"songloft/internal/services/resourcebudget"
)

// budgetSlots 读某分区的实时活跃数（预算表取代了原来的包级信号量）。
func budgetSlots(t *testing.T, partition string) int {
	t.Helper()
	rep, ok := resourcebudget.ProcessBudget().Report()[partition]
	if !ok {
		t.Fatalf("预算表缺分区 %q", partition)
	}
	return rep["active"]
}

func budgetTotalActive(t *testing.T) int {
	t.Helper()
	return resourcebudget.ProcessBudget().Report()["_total"]["active"]
}

// fillSeekBudget 预占 seek 分区的整整配额（饱和注入），返回的函数把它们全部归还。
func fillSeekBudget(t *testing.T) func() {
	t.Helper()
	maxActive := resourcebudget.DefaultBudget().Report()[seekStreamPartition]["max_active"]
	held := make([]func(), 0, maxActive)
	for i := 0; i < maxActive; i++ {
		rel, err := resourcebudget.ProcessBudget().TryAcquire(seekStreamPartition)
		if err != nil {
			for _, r := range held {
				r()
			}
			t.Fatalf("预占 %s 分区第 %d 个槽失败: %v", seekStreamPartition, i+1, err)
		}
		held = append(held, rel)
	}
	return func() {
		for _, r := range held {
			r()
		}
	}
}

func TestHLSTranscodeSlotSaturationVisible(t *testing.T) {
	maxActive := resourcebudget.DefaultBudget().Report()[seekStreamPartition]["max_active"]
	if maxActive <= 0 {
		t.Fatalf("seek 分区容量配置异常: %d", maxActive)
	}
	// 预算表是进程级单例：先确认进入测试时没有别的工作占着槽位。
	if n := budgetSlots(t, seekStreamPartition); n != 0 {
		t.Fatalf("测试前置失败：seek 分区已有 %d 个占用", n)
	}
	if n := budgetTotalActive(t); n != 0 {
		t.Fatalf("测试前置失败：进程总活跃已有 %d", n)
	}
	// 上游只允许回环网关；外部环境代理不能截获 ffmpeg 到网关的请求。
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("no_proxy", "")

	file := hlsFixture(t, "libx264", 60)
	upstream := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(file))))
	defer upstream.Close()
	s := hlsTestService(t)
	opts := HLSTranscodeOptions{URL: upstream.URL + "/source.ts"}

	// 1) 装满全部分区槽位：每个 HLS 会话在生命周期内持续持有槽位。
	ids := make([]string, 0, maxActive)
	for i := 0; i < maxActive; i++ {
		id, err := s.Create(context.Background(), "owner", opts)
		if err != nil {
			t.Fatalf("第 %d 个会话（容量内）失败: %v", i+1, err)
		}
		ids = append(ids, id)
		t.Logf("会话 %d 创建成功 slots=%d/%d", i+1, budgetSlots(t, seekStreamPartition), maxActive)
	}
	if n := budgetSlots(t, seekStreamPartition); n != maxActive {
		t.Fatalf("slots=%d want %d", n, maxActive)
	}

	// 2) 超容量请求：必须快速、明确失败，且错误码/文案可被客户端识别。
	start := time.Now()
	extra, err := s.Create(context.Background(), "owner", opts)
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Delete(extra, "owner")
		t.Fatal("槽饱和时 Create 静默成功（不可接受的故障注入结果）")
	}
	if got := HLSTranscodeStatus(err); got != 503 {
		t.Fatalf("超容量错误码=%d want 503, err=%v", got, err)
	}
	if !strings.Contains(err.Error(), "转码并发已满") {
		t.Fatalf("错误文案不可识别: %v", err)
	}
	t.Logf("饱和拒绝: status=%d elapsed=%v message=%q", HLSTranscodeStatus(err), elapsed, err.Error())
	if elapsed > 2*time.Second {
		t.Fatalf("超容量请求耗时 %v：疑似排队而非快速拒绝", elapsed)
	}
	if n := budgetSlots(t, seekStreamPartition); n != maxActive {
		t.Fatalf("被拒绝的请求泄漏/改变了槽位: slots=%d", n)
	}

	// 3) 释放一个会话 → 槽位归还 → 新会话可再次成功。
	if err := s.Delete(ids[0], "owner"); err != nil {
		t.Fatalf("释放会话失败: %v", err)
	}
	if n := budgetSlots(t, seekStreamPartition); n != maxActive-1 {
		t.Fatalf("Delete 后 slots=%d want %d（槽位未释放）", n, maxActive-1)
	}
	t.Logf("释放 1 个会话后 slots=%d/%d", budgetSlots(t, seekStreamPartition), maxActive)
	reused, err := s.Create(context.Background(), "owner", opts)
	if err != nil {
		t.Fatalf("槽位释放后 Create 仍失败: %v", err)
	}
	ids = append(ids[1:], reused)
	if n := budgetSlots(t, seekStreamPartition); n != maxActive {
		t.Fatalf("复用后 slots=%d want %d", n, maxActive)
	}

	// 4) 全部结束 → 分区与进程总活跃归零（不泄漏）。
	for _, id := range ids {
		if err := s.Delete(id, "owner"); err != nil {
			t.Fatalf("清理会话 %s: %v", id, err)
		}
	}
	if n := budgetSlots(t, seekStreamPartition); n != 0 {
		t.Fatalf("全部结束后 slots=%d want 0", n)
	}
	if n := budgetTotalActive(t); n != 0 {
		t.Fatalf("全部结束后进程总活跃=%d want 0", n)
	}
}

// TestBudgetGlobalCapAcrossPartitions 与 TestBudgetBackgroundReserve 是预算原语自身的
// 契约，放在 resourcebudget 包（不依赖 ffmpeg）。本文件只验 HLS 会话接入后的饱和表现。
