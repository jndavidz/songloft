package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// fullBody 读取完整响应体。
func fullBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestFixedServe_RFC9110：固定表示语义——416 带 bytes */N、If-Range 失配回
// 完整同一表示、多段 Range 回完整 200、多切片拼接哈希等于全文件、冷暖同字节。
func TestFixedServe_RFC9110(t *testing.T) {
	iss, ms, rep := testIssuer(t)
	srv := wrapMedia(t, ms)
	tk := issueFor(t, iss, rep)
	url := "/media?media_ticket=" + tk.Token

	var full []byte
	etag := ""

	t.Run("全量与 ETag", func(t *testing.T) {
		resp := getMedia(t, srv, url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		full = fullBody(t, resp)
		etag = resp.Header.Get("ETag")
		if etag == "" {
			t.Fatal("固定表示必须带 ETag（冷暖同表示身份）")
		}
	})
	t.Run("多切片拼接哈希等于全文件", func(t *testing.T) {
		slices := []string{"bytes=0-99", "bytes=100-199", "bytes=1000-1099"}
		var cat []byte
		for _, rg := range slices {
			req, _ := http.NewRequest(http.MethodGet, srv.url+url, nil)
			req.Header.Set("Range", rg)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusPartialContent {
				t.Fatalf("%s: 应 206，got %d", rg, resp.StatusCode)
			}
			body := fullBody(t, resp)
			start, _ := strconv.Atoi(strings.TrimPrefix(strings.Split(rg, "-")[0], "bytes="))
			end, _ := strconv.Atoi(strings.Split(rg, "-")[1])
			if len(body) != end-start+1 {
				t.Fatalf("%s: 切片长度 %d ≠ %d", rg, len(body), end-start+1)
			}
			if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-%d/", start, end)) {
				t.Fatalf("%s: Content-Range=%q", rg, cr)
			}
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
				t.Fatalf("%s: Content-Length=%q 实际 %d", rg, cl, len(body))
			}
			// 逐字节对照同一表示的对应区间（拼接语义 = 同一对象的区间）
			for i, b := range body {
				if full[start+i] != b {
					t.Fatalf("%s: 第 %d 字节与全文件不符", rg, i)
				}
			}
			cat = append(cat, body...)
		}
		if len(cat) != 300 {
			t.Errorf("三切片合计应 300 字节，got %d", len(cat))
		}
	})
	t.Run("不可满足 Range → 416 且 bytes */N", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.url+url, nil)
		req.Header.Set("Range", "bytes=99999999-")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("应 416，got %d", resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes */%d", len(full)) {
			t.Errorf("Content-Range 应 bytes */%d，got %q", len(full), cr)
		}
	})
	t.Run("If-Range 失配 → 完整同一表示", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.url+url, nil)
		req.Header.Set("Range", "bytes=0-99")
		req.Header.Set("If-Range", `"stale-etag"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body := fullBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("If-Range 失配应 200 完整，got %d", resp.StatusCode)
		}
		if sha(body) != sha(full) {
			t.Error("If-Range 失配必须返回完整同一表示（不得拼接另一对象）")
		}
	})
	t.Run("If-Range 匹配 → 206", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.url+url, nil)
		req.Header.Set("Range", "bytes=0-99")
		req.Header.Set("If-Range", etag)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("If-Range 匹配应 206，got %d", resp.StatusCode)
		}
	})
	t.Run("多段 Range → 200 完整同一表示", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.url+url, nil)
		req.Header.Set("Range", "bytes=0-99,200-299")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body := fullBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("多段 Range 应回退 200 完整，got %d", resp.StatusCode)
		}
		if sha(body) != sha(full) {
			t.Error("多段 Range 回退必须是同一表示的完整字节")
		}
	})
	t.Run("冷暖同表示（重复请求字节一致）", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			resp := getMedia(t, srv, url)
			if sha(fullBody(t, resp)) != sha(full) {
				t.Errorf("第 %d 次请求字节不一致（冷暖必须同一表示）", i)
			}
			if e := resp.Header.Get("ETag"); e != etag {
				t.Errorf("ETag 漂移: %q → %q", etag, e)
			}
		}
	})
}
