package managed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"songloft/internal/httputil"
)

// 真实下载器（S-03）：完整下载须限制实际累计字节和总时间，不能只信
// Content-Length；上游 headers 仅用于解析结果声明的源站；逐跳重定向重新
// 校验目标，Cookie/Authorization 不跟随到任意域名。

// DownloadLimits 约束一次完整下载。
type DownloadLimits struct {
	MaxBytes     int64         // 实际累计字节上限（0 = 用默认 256MiB）
	TotalTimeout time.Duration // 总时间上限（0 = 用默认 10min）
	HTTPClient   *http.Client  // nil = httputil.NewDownloadClient()（全局代理感知）
	// HostChecker 逐跳校验目标主机（含重定向后的每一跳）；nil = 不校验（仅测试用）。
	// 生产接线用 services.IsHostnameAllowed 封禁私网/回环（SSRF 防线）。
	HostChecker func(host string) error
}

const (
	defaultMaxBytes     = 256 << 20
	defaultTotalTimeout = 10 * time.Minute
)

// DownloadToFile 把 result.URL 拉取到 destDir/source.<ext>。
// 返回落盘路径与实际字节数。任何上限触碰、非 2xx、或重定向到非 HTTP(S)
// 目标都失败——失败不返回部分文件语义。
func DownloadToFile(ctx context.Context, result *ResolveResult, destDir string, limits DownloadLimits) (string, int64, error) {
	if result == nil || result.URL == "" {
		return "", 0, fmt.Errorf("download: empty media url")
	}
	maxBytes := limits.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	timeout := limits.TotalTimeout
	if timeout <= 0 {
		timeout = defaultTotalTimeout
	}
	client := limits.HTTPClient
	if client == nil {
		// 全局代理感知的下载 client（pitfalls「通用 HTTP Proxy」：外发请求
		// 一律走 httputil 构造的 client，不能自建裸 client 绕过代理）。
		client = httputil.NewDownloadClient()
	}
	// 重定向逐跳自管：强制 client 把 3xx 原样交回，凭证跟随策略由本函数裁决
	// （不能让 http.Client 的默认重定向把上游 headers 带到任意目标）。
	follower := *client
	follower.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client = &follower
	dlCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	current := result.URL
	authHeaders := result.Headers // 只带给定源站
	for hop := 0; ; hop++ {
		if hop > 5 {
			return "", 0, fmt.Errorf("download: too many redirects")
		}
		u, err := url.Parse(current)
		if err != nil {
			return "", 0, fmt.Errorf("download: bad url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", 0, fmt.Errorf("download: refused non-http(s) scheme %q", u.Scheme)
		}
		if limits.HostChecker != nil {
			if err := limits.HostChecker(u.Hostname()); err != nil {
				return "", 0, fmt.Errorf("download: host %q refused: %w", u.Hostname(), err)
			}
		}
		req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, current, nil)
		if err != nil {
			return "", 0, err
		}
		for k, v := range authHeaders {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, fmt.Errorf("download: %w", err)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", 0, fmt.Errorf("download: %d redirect without Location", resp.StatusCode)
			}
			next, err := u.Parse(loc)
			if err != nil {
				return "", 0, fmt.Errorf("download: bad redirect target: %w", err)
			}
			// 凭证只跟随同源；跨源重定向丢弃全部认证头（防 SSRF 凭证外带）。
			if next.Host != u.Host {
				authHeaders = nil
			}
			current = next.String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return "", 0, fmt.Errorf("download: http %d", resp.StatusCode)
		}

		if err := os.MkdirAll(destDir, 0o755); err != nil {
			resp.Body.Close()
			return "", 0, err
		}
		ext := strings.TrimPrefix(result.Ext, ".")
		if ext == "" {
			ext = "bin"
		}
		dstPath := filepath.Join(destDir, "source."+ext)
		dst, err := os.Create(dstPath)
		if err != nil {
			resp.Body.Close()
			return "", 0, err
		}
		// 实际累计字节为准：Content-Length 可以撒谎，CountingWriter 不可以。
		n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
		closeErr := dst.Close()
		resp.Body.Close()
		if err != nil {
			os.Remove(dstPath)
			return "", 0, fmt.Errorf("download: %w", err)
		}
		if closeErr != nil {
			os.Remove(dstPath)
			return "", 0, closeErr
		}
		if n > maxBytes {
			os.Remove(dstPath)
			return "", 0, fmt.Errorf("download: exceeded byte cap %d (declared length lied or stream runaway)", maxBytes)
		}
		return dstPath, n, nil
	}
}
