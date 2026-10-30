package services

// 受控故障注入：HLS 转码槽饱和。
//
// 契约（hls_transcode.go Create / seek_stream.go seekStreamMaxConcurrent）：
//  1. seekStreamSem 是包级全局闸门，容量 seekStreamMaxConcurrent（当前 4），
//     HLS 会话在整个生命周期内持有 1 个槽位；
//  2. 槽位满时 Create 必须 **立即** 返回明确错误（503 "转码并发已满"），
//     既不排队（无界等待）也不静默成功；
//  3. 任一存活会话结束（Delete / 失败 / 闲置回收）必须归还槽位，
//     后续 Create 可以重新成功——槽位不泄漏。
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
)

func TestHLSTranscodeSlotSaturationVisible(t *testing.T) {
	if got, want := cap(seekStreamSem), seekStreamMaxConcurrent; got != want {
		t.Fatalf("seekStreamSem cap=%d, want %d（容量契约已变，请同步本测试）", got, want)
	}
	// 包级信号量是进程内全局：先确认进入测试时没有别的会话占着槽位。
	if n := len(seekStreamSem); n != 0 {
		t.Fatalf("测试前置失败：seekStreamSem 已有 %d 个占用", n)
	}
	// 上游只允许回环网关；外部环境代理不能截获 ffmpeg 到网关的请求。
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("no_proxy", "")

	file := hlsFixture(t, "libx264", 60)
	upstream := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(file))))
	defer upstream.Close()
	s := hlsTestService(t)
	opts := HLSTranscodeOptions{URL: upstream.URL + "/source.ts"}

	// 1) 装满全部槽位：每个 HLS 会话在生命周期内持续持有槽位。
	ids := make([]string, 0, cap(seekStreamSem))
	for i := 0; i < cap(seekStreamSem); i++ {
		id, err := s.Create(context.Background(), "owner", opts)
		if err != nil {
			t.Fatalf("第 %d 个会话（容量内）失败: %v", i+1, err)
		}
		ids = append(ids, id)
		t.Logf("会话 %d 创建成功 slots=%d/%d", i+1, len(seekStreamSem), cap(seekStreamSem))
	}
	if n := len(seekStreamSem); n != cap(seekStreamSem) {
		t.Fatalf("slots=%d want %d", n, cap(seekStreamSem))
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
	if n := len(seekStreamSem); n != cap(seekStreamSem) {
		t.Fatalf("被拒绝的请求泄漏/改变了槽位: slots=%d", n)
	}

	// 3) 释放一个会话 → 槽位归还 → 新会话可再次成功。
	if err := s.Delete(ids[0], "owner"); err != nil {
		t.Fatalf("释放会话失败: %v", err)
	}
	if n := len(seekStreamSem); n != cap(seekStreamSem)-1 {
		t.Fatalf("Delete 后 slots=%d want %d（槽位未释放）", n, cap(seekStreamSem)-1)
	}
	t.Logf("释放 1 个会话后 slots=%d/%d", len(seekStreamSem), cap(seekStreamSem))
	reused, err := s.Create(context.Background(), "owner", opts)
	if err != nil {
		t.Fatalf("槽位释放后 Create 仍失败: %v", err)
	}
	ids = append(ids[1:], reused)
	if n := len(seekStreamSem); n != cap(seekStreamSem) {
		t.Fatalf("复用后 slots=%d want %d", n, cap(seekStreamSem))
	}

	// 4) 全部结束 → 槽位归零（不泄漏）。
	for _, id := range ids {
		if err := s.Delete(id, "owner"); err != nil {
			t.Fatalf("清理会话 %s: %v", id, err)
		}
	}
	if n := len(seekStreamSem); n != 0 {
		t.Fatalf("全部结束后 slots=%d want 0", n)
	}
}
