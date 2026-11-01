package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"songloft/internal/httputil"
)

// rtFunc 把函数适配成 http.RoundTripper。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newFakeClient 返回一个直接吐预设 body 的 client,用于在不经真实网络栈的前提下
// 构造"body 干净 EOF 却短于 Content-Length"这种真实网络栈会自行拦成 ErrUnexpectedEOF、
// 从而无法覆盖到 downloadToTemp 显式截断检查的场景。
func newFakeClient(body []byte, contentLength int64) *http.Client {
	return &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: contentLength,
			Header:        make(http.Header),
		}, nil
	})}
}

func newFetcherWithClient(c *http.Client) *SourceFetcher {
	return NewSourceFetcher(FetcherOpts{HTTPClient: c})
}

// newRangeClient 返回一个按 Range 头分块响应(206)的 client,模拟 googlevideo 等支持
// Range 的 CDN。用于覆盖 downloadToTemp 的分块拉取路径(issue #305)。
func newRangeClient(full []byte, t *testing.T) (*http.Client, *int) {
	requests := 0
	total := int64(len(full))
	c := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			t.Fatalf("expected Range header, got none")
		}
		var start, end int64
		if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
			t.Fatalf("bad Range header %q: %v", rangeHdr, err)
		}
		if end > total-1 {
			end = total - 1
		}
		chunk := full[start : end+1]
		h := make(http.Header)
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Body:          io.NopCloser(bytes.NewReader(chunk)),
			ContentLength: int64(len(chunk)),
			Header:        h,
		}, nil
	})}
	return c, &requests
}

// 支持 Range 的服务端 → 分块顺序拉取并正确拼接完整文件(issue #305)。
func TestDownloadToTemp_RangeChunked(t *testing.T) {
	// 造 25MiB,在 10MiB 分块下需要 3 个 Range 请求(10+10+5)。
	full := bytes.Repeat([]byte("z"), int(httputil.DownloadChunkSize*2+httputil.DownloadChunkSize/2))
	c, requests := newRangeClient(full, t)
	f := newFetcherWithClient(c)

	path, n, err := f.downloadToTemp(context.Background(), "http://x/audio", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(path)
	if n != int64(len(full)) {
		t.Fatalf("expected %d bytes, got %d", len(full), n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("downloaded content mismatch: got %d bytes", len(got))
	}
	if *requests != 3 {
		t.Fatalf("expected 3 range requests, got %d", *requests)
	}
}

// Content-Length 声明 1000 但只吐 500 字节 → 判为截断,报错且不留临时文件。
func TestDownloadToTemp_Truncated(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 500)
	f := newFetcherWithClient(newFakeClient(body, 1000))

	path, n, err := f.downloadToTemp(context.Background(), "http://x/audio", nil)
	if err == nil {
		t.Fatalf("expected truncation error, got path=%q n=%d", path, n)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected truncated error, got %v", err)
	}
	if path != "" {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("temp file should be removed on truncation: %s", path)
		}
	}
}

// Content-Length 未知(-1,chunked / gzip 透明解压)→ 不误判,下载成功。
func TestDownloadToTemp_UnknownLengthNoFalsePositive(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 500)
	f := newFetcherWithClient(newFakeClient(body, -1))

	path, n, err := f.downloadToTemp(context.Background(), "http://x/audio", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(path)
	if n != 500 {
		t.Fatalf("expected 500 bytes, got %d", n)
	}
}

// Content-Length 与实际一致 → 成功。
func TestDownloadToTemp_ExactLength(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 500)
	f := newFetcherWithClient(newFakeClient(body, 500))

	path, n, err := f.downloadToTemp(context.Background(), "http://x/audio", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(path)
	if n != 500 {
		t.Fatalf("expected 500 bytes, got %d", n)
	}
}

// capturingInvoker 捕获 music/url 请求体并返回预置响应,用于断言 quality hint 的下发形态。
type capturingInvoker struct {
	respBody []byte
	gotBody  []byte
}

func (c *capturingInvoker) InvokeHTTP(
	_ context.Context,
	_, _, _ string,
	_ interface{},
	body []byte,
) (int, map[string]string, []byte, error) {
	c.gotBody = body
	return http.StatusOK, nil, c.respBody, nil
}

// 播放链路携带的 quality hint 原样进入 /api/music/url 请求体；上游附带的
// bitrate_kbps(实测码率)被容忍并解码成功(仅日志用途,不改变解析结果)。
func TestResolveURL_QualityHintForwardedInRequestBody(t *testing.T) {
	inv := &capturingInvoker{respBody: []byte(`{"url":"https://cdn/x.mp3","bitrate_kbps":320}`)}
	f := NewSourceFetcher(FetcherOpts{PluginInvoker: inv})
	song := &SongInfo{Title: "Song", Artist: "Artist", PluginEntryPath: "p1"}

	res, err := f.ResolveURL(context.Background(), "p1", `{"id":"1"}`, song, true, "320k")
	if err != nil {
		t.Fatalf("ResolveURL: %v", err)
	}
	if res.URL != "https://cdn/x.mp3" {
		t.Fatalf("unexpected url: %q", res.URL)
	}

	var body map[string]any
	if err := json.Unmarshal(inv.gotBody, &body); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	if body["quality"] != "320k" {
		t.Fatalf("expected quality hint forwarded, got %v", body["quality"])
	}
}

// 未携带 hint(缓存下载/元数据探测路径)时请求体不含 quality 字段,与旧版逐字节兼容。
func TestResolveURL_EmptyQualityHintOmitsField(t *testing.T) {
	inv := &capturingInvoker{respBody: []byte(`{"url":"https://cdn/x.mp3"}`)}
	f := NewSourceFetcher(FetcherOpts{PluginInvoker: inv})
	song := &SongInfo{Title: "Song", Artist: "Artist", PluginEntryPath: "p1"}

	if _, err := f.ResolveURL(context.Background(), "p1", `{"id":"1"}`, song, true, ""); err != nil {
		t.Fatalf("ResolveURL: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(inv.gotBody, &body); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	if _, present := body["quality"]; present {
		t.Fatalf("expected no quality field when hint empty, got %v", body["quality"])
	}
}
