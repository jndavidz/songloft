package source

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeFetcher 按预设脚本依次返回结果,记录被调用次数,供 orchestrator 重试逻辑单测。
type fakeFetcher struct {
	calls   int
	results []fakeResult // 第 i 次 Fetch 返回 results[i](越界则用最后一个)
}

type fakeResult struct {
	res *FetchResult
	err error
}

func (f *fakeFetcher) Fetch(_ context.Context, _, _ string, _ *SongInfo, _ bool) (*FetchResult, error) {
	i := f.calls
	f.calls++
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	r := f.results[i]
	return r.res, r.err
}

func (f *fakeFetcher) ResolveURL(_ context.Context, _, _ string, _ *SongInfo, _ bool, _ string) (*ResolvedURL, error) {
	return nil, errors.New("not used")
}

func newTestOrchestrator(f *fakeFetcher, retries int) *SourceOrchestrator {
	return NewSourceOrchestrator(OrchestratorOpts{
		Fetcher:                f,
		SameSourceRetries:      retries,
		SameSourceRetryBackoff: time.Millisecond, // 测试里退避尽量短
	})
}

func testSong() *SongInfo {
	return &SongInfo{ID: 1, PluginEntryPath: "bili", SourceData: `{"bvid":"x"}`}
}

// 前 N 次可重试失败,最后一次成功 → 应重试到成功。
func TestFetch_RetryThenSuccess(t *testing.T) {
	ok := &FetchResult{TempPath: "/tmp/ok", PluginEntryPath: "bili", SourceData: `{"bvid":"x"}`}
	f := &fakeFetcher{results: []fakeResult{
		{nil, &NetworkError{Op: "read", Err: errors.New("truncated")}},
		{nil, &InvalidAudioError{Reason: ReasonDurationMismatchLow, Expected: 319, Actual: 220}},
		{ok, nil},
	}}
	o := newTestOrchestrator(f, 2) // 最多 1+2=3 次

	res, err := o.Fetch(context.Background(), testSong(), ModeStrict)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res != ok {
		t.Fatalf("unexpected result: %+v", res)
	}
	if f.calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", f.calls)
	}
}

// 全程可重试失败 → 用尽重试次数后返回最后错误(ModeStrict 不进 L2)。
func TestFetch_RetryExhausted(t *testing.T) {
	f := &fakeFetcher{results: []fakeResult{
		{nil, &NetworkError{Op: "read", Err: errors.New("truncated")}},
	}}
	o := newTestOrchestrator(f, 2)

	_, err := o.Fetch(context.Background(), testSong(), ModeStrict)
	if err == nil {
		t.Fatal("expected error")
	}
	var ne *NetworkError
	if !errors.As(err, &ne) {
		t.Fatalf("expected NetworkError, got %v", err)
	}
	if f.calls != 3 { // 1 + 2 retries
		t.Fatalf("expected 3 attempts, got %d", f.calls)
	}
}

// 不可重试错误(duration_mismatch_high)→ 只尝试一次,不重试。
func TestFetch_NonRetryableNoRetry(t *testing.T) {
	f := &fakeFetcher{results: []fakeResult{
		{nil, &InvalidAudioError{Reason: ReasonDurationMismatchHigh, Expected: 100, Actual: 300}},
	}}
	o := newTestOrchestrator(f, 2)

	_, err := o.Fetch(context.Background(), testSong(), ModeStrict)
	if err == nil {
		t.Fatal("expected error")
	}
	if f.calls != 1 {
		t.Fatalf("expected 1 attempt (no retry), got %d", f.calls)
	}
}

// ctx 已取消 → 首次尝试后不再重试等待。
func TestFetch_CtxCancelStopsRetry(t *testing.T) {
	f := &fakeFetcher{results: []fakeResult{
		{nil, &NetworkError{Op: "read", Err: errors.New("truncated")}},
	}}
	// 退避设长,确保 ctx 取消在退避处被感知
	o := NewSourceOrchestrator(OrchestratorOpts{
		Fetcher:                f,
		SameSourceRetries:      3,
		SameSourceRetryBackoff: 10 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	start := time.Now()
	_, err := o.Fetch(ctx, testSong(), ModeStrict)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("retry did not respect ctx cancel, took %v", elapsed)
	}
	if f.calls != 1 { // 只跑首次,退避处因 ctx 取消退出
		t.Fatalf("expected 1 attempt, got %d", f.calls)
	}
}
