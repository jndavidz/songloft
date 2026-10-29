package managed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDownload_BasicAndByteCap：正常落盘；超限按实际累计字节拒绝（不信 Content-Length）。
func TestDownload_BasicAndByteCap(t *testing.T) {
	payload := strings.Repeat("A", 1024)
	t.Run("正常下载", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, payload)
		}))
		defer srv.Close()
		got, n, err := DownloadToFile(context.Background(),
			&ResolveResult{URL: srv.URL, Ext: "mp3"}, t.TempDir(), DownloadLimits{})
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		if n != int64(len(payload)) || filepath.Base(got) != "source.mp3" {
			t.Errorf("got %s n=%d", got, n)
		}
	})
	t.Run("实际字节超限拒绝且不留部分文件", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// chunked 流式输出且无 Content-Length——上游只能靠实际字节约束，
			// 模拟「Content-Length 缺失/不可信」的真实流
			flusher := w.(http.Flusher)
			for i := 0; i < 4; i++ {
				fmt.Fprint(w, payload)
				flusher.Flush()
			}
		}))
		defer srv.Close()
		dir := t.TempDir()
		_, _, err := DownloadToFile(context.Background(),
			&ResolveResult{URL: srv.URL, Ext: "mp3"}, dir, DownloadLimits{MaxBytes: 1024})
		if err == nil || !strings.Contains(err.Error(), "byte cap") {
			t.Fatalf("应按实际字节拒绝，got %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "source.mp3")); !os.IsNotExist(statErr) {
			t.Error("失败不得留下部分文件")
		}
	})
	t.Run("非 2xx 拒绝", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if _, _, err := DownloadToFile(context.Background(),
			&ResolveResult{URL: srv.URL, Ext: "mp3"}, t.TempDir(), DownloadLimits{}); err == nil {
			t.Fatal("403 应失败")
		}
	})
	t.Run("非 http scheme 拒绝", func(t *testing.T) {
		if _, _, err := DownloadToFile(context.Background(),
			&ResolveResult{URL: "file:///etc/passwd", Ext: "mp3"}, t.TempDir(), DownloadLimits{}); err == nil {
			t.Fatal("file:// 应拒绝")
		}
	})
	t.Run("总时间超限拒绝", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(500 * time.Millisecond)
			fmt.Fprint(w, payload)
		}))
		defer srv.Close()
		start := time.Now()
		_, _, err := DownloadToFile(context.Background(),
			&ResolveResult{URL: srv.URL, Ext: "mp3"}, t.TempDir(), DownloadLimits{TotalTimeout: 50 * time.Millisecond})
		if err == nil {
			t.Fatal("超时应失败")
		}
		if time.Since(start) > 2*time.Second {
			t.Error("总时间上限应生效，而非等待完整响应")
		}
	})
}

// TestDownload_RedirectCredentialHandling：跨源重定向丢弃认证头；同源保留。
func TestDownload_RedirectCredentialHandling(t *testing.T) {
	var sawAuthOnTarget atomic.Bool
	var sawAuthOnSameHost atomic.Bool

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "" && strings.Contains(r.URL.Path, "final") {
			if r.Header.Get("X-Upstream-Cookie") != "" {
				sawAuthOnTarget.Store(true)
			}
			fmt.Fprint(w, "final-bytes")
		}
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Upstream-Cookie") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/same-host") {
			sawAuthOnSameHost.Store(true)
			fmt.Fprint(w, "same-host-bytes")
			return
		}
		// 跨源重定向
		http.Redirect(w, r, target.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	src := &ResolveResult{URL: srv.URL + "/start", Ext: "mp3",
		Headers: map[string]string{"X-Upstream-Cookie": "secret=1"}}

	t.Run("跨源重定向丢凭证", func(t *testing.T) {
		_, _, err := DownloadToFile(context.Background(), src, t.TempDir(), DownloadLimits{})
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		if sawAuthOnTarget.Load() {
			t.Error("认证头不得跟随跨源重定向（SSRF 凭证外带）")
		}
	})
	t.Run("同源路径保留凭证", func(t *testing.T) {
		same := &ResolveResult{URL: srv.URL + "/same-host", Ext: "mp3",
			Headers: map[string]string{"X-Upstream-Cookie": "secret=1"}}
		_, _, err := DownloadToFile(context.Background(), same, t.TempDir(), DownloadLimits{})
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		if !sawAuthOnSameHost.Load() {
			t.Error("同源请求应携带声明的上游 headers")
		}
	})
	t.Run("重定向环拒绝", func(t *testing.T) {
		var loopURL atomic.Value
		loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, loopURL.Load().(string)+"/a", http.StatusFound)
		}))
		defer loop.Close()
		loopURL.Store(loop.URL)
		if _, _, err := DownloadToFile(context.Background(),
			&ResolveResult{URL: loop.URL, Ext: "mp3"}, t.TempDir(), DownloadLimits{}); err == nil {
			t.Fatal("重定向环应拒绝")
		}
	})
}
