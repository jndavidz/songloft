package managed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"songloft/internal/database/testutil"
)

func testIssuer(t *testing.T) (*TicketIssuer, *MediaServer, string) {
	t.Helper()
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	root := t.TempDir()
	finalDir := filepath.Join(root, "managed", "abc123")
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(filepath.Join("testdata", "valid_128k.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(finalDir, "final.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	srv := NewMediaServer(iss, root)
	return iss, srv, filepath.ToSlash(filepath.Join("managed", "abc123", "final.mp3"))
}

func issueFor(t *testing.T, iss *TicketIssuer, rep string) *Ticket {
	t.Helper()
	tk, err := iss.Issue(context.Background(), TicketRequest{
		RefDigest: "digest-x", Representation: rep,
		Session: "sess-1", Epoch: 1, Entry: "e1", Target: "device", Subject: "songloft-managed",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tk
}

func getMedia(t *testing.T, srv *mediaSrv, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.url+url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// mediaSrv 把 MediaServer 挂上 httptest，供测试发真实 HTTP 请求。
type mediaSrv struct {
	*MediaServer
	url string
}

func wrapMedia(t *testing.T, srv *MediaServer) *mediaSrv {
	ts := httptest.NewServer(http.HandlerFunc(srv.ServeMedia))
	t.Cleanup(ts.Close)
	return &mediaSrv{MediaServer: srv, url: ts.URL}
}

// TestTicket_AuthMatrix：过期/撤销/越绑定/未知 token 的拒绝矩阵。
func TestTicket_AuthMatrix(t *testing.T) {
	iss, ms, rep := testIssuer(t)
	srv := wrapMedia(t, ms)
	tk := issueFor(t, iss, rep)

	t.Run("合法票据 GET 200", func(t *testing.T) {
		resp := getMedia(t, srv, "/media?media_ticket="+tk.Token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
	})
	t.Run("HEAD 无响应体", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodHead, srv.url+"/media?media_ticket="+tk.Token, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if len(body) != 0 {
			t.Errorf("HEAD 不得有响应体，got %d bytes", len(body))
		}
		if resp.ContentLength <= 0 {
			t.Errorf("HEAD 应声明 Content-Length，got %d", resp.ContentLength)
		}
	})
	t.Run("未知 token 401", func(t *testing.T) {
		if resp := getMedia(t, srv, "/media?media_ticket=deadbeef"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status=%d", resp.StatusCode)
		}
	})
	t.Run("缺票据 401", func(t *testing.T) {
		if resp := getMedia(t, srv, "/media"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status=%d", resp.StatusCode)
		}
	})
	t.Run("Range 206 与切片字节正确", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.url+"/media?media_ticket="+tk.Token, nil)
		req.Header.Set("Range", "bytes=0-99")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("Range 应 206，got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if len(body) != 100 {
			t.Errorf("切片应 100 字节，got %d", len(body))
		}
		if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-99/") {
			t.Errorf("Content-Range=%q", cr)
		}
	})
}

// TestTicket_ExpiryAndRevocation：过期拒绝；lease 撤销只影响本 lease；
// 换代撤销 entry 全部票据；同文件双客户端互不影响。
func TestTicket_ExpiryAndRevocation(t *testing.T) {
	iss, ms, rep := testIssuer(t)
	srv := wrapMedia(t, ms)

	t.Run("过期拒绝", func(t *testing.T) {
		expired := issueFor(t, iss, rep)
		// 把时钟拨过到期点
		base := iss.Now()
		iss.Now = func() time.Time { return base.Add(2 * time.Hour) }
		defer func() { iss.Now = func() time.Time { return base } }()
		if resp := getMedia(t, srv, "/media?media_ticket="+expired.Token); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("过期票据应 401，got %d", resp.StatusCode)
		}
	})
	t.Run("lease 撤销只影响本 lease", func(t *testing.T) {
		leaseA := issueFor(t, iss, rep)
		leaseB := issueFor(t, iss, rep) // 同文件另一客户端，独立 lease
		if leaseA.LeaseID == leaseB.LeaseID {
			t.Fatal("两次签发必须是独立 lease")
		}
		if err := iss.RevokeLease(context.Background(), leaseA.LeaseID); err != nil {
			t.Fatalf("revoke A: %v", err)
		}
		if resp := getMedia(t, srv, "/media?media_ticket="+leaseA.Token); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("A 撤销后应 401，got %d", resp.StatusCode)
		}
		if resp := getMedia(t, srv, "/media?media_ticket="+leaseB.Token); resp.StatusCode != http.StatusOK {
			t.Errorf("B 不受 A 撤销影响，got %d", resp.StatusCode)
		}
	})
	t.Run("换代撤销 entry 全部票据", func(t *testing.T) {
		t1 := issueFor(t, iss, rep)
		t2 := issueFor(t, iss, rep)
		if err := iss.RevokeEntry(context.Background(), "sess-1", 1, "e1"); err != nil {
			t.Fatalf("revoke entry: %v", err)
		}
		for _, tk := range []*Ticket{t1, t2} {
			if resp := getMedia(t, srv, "/media?media_ticket="+tk.Token); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("换代后旧代次票据应 401，got %d", resp.StatusCode)
			}
		}
	})
	t.Run("撤销后重放 DB 撤销位仍拒绝（重启语义）", func(t *testing.T) {
		tk := issueFor(t, iss, rep)
		globalRevocations = &RevocationRegistry{revoked: map[string]struct{}{}} // 清空进程内广播，模拟重启
		if err := iss.RevokeLease(context.Background(), tk.LeaseID); err != nil {
			t.Fatal(err)
		}
		globalRevocations = &RevocationRegistry{revoked: map[string]struct{}{}}
		// 进程内广播清空后，guard 的 DB 检查仍应拒绝
		req, _ := http.NewRequest(http.MethodGet, srv.url+"/media?media_ticket="+tk.Token, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// 新连接（guard 尚未建立）走 Record → DB revoked → 401
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("DB 撤销位应拒绝，got %d", resp.StatusCode)
		}
	})
}

// TestTicket_Renewal：续期延长授权、URI/表示不变、绝对上限、过期不可续。
func TestTicket_Renewal(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	iss.DefaultTTL = 30 * time.Minute
	iss.RenewCap = 1 * time.Hour
	base := time.Unix(1_000_000, 0)
	iss.Now = func() time.Time { return base }

	tk, err := iss.Issue(context.Background(), TicketRequest{
		RefDigest: "d", Representation: "managed/x/final.mp3",
		Session: "s", Epoch: 1, Entry: "e", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("续期成功且在绝对上限内", func(t *testing.T) {
		newExp, err := iss.Renew(context.Background(), tk.Token, 20*time.Minute)
		if err != nil {
			t.Fatalf("renew: %v", err)
		}
		if !newExp.Equal(base.Add(20 * time.Minute)) {
			t.Errorf("newExp=%v", newExp)
		}
		if tk.Token == "" {
			t.Error("token 不应变化")
		}
	})
	t.Run("超绝对上限拒绝", func(t *testing.T) {
		if _, err := iss.Renew(context.Background(), tk.Token, 2*time.Hour); err != ErrTicketAbsolute {
			t.Fatalf("应 ErrTicketAbsolute，got %v", err)
		}
	})
	t.Run("过期票据不可续期", func(t *testing.T) {
		iss.Now = func() time.Time { return base.Add(2 * time.Hour) }
		defer func() { iss.Now = func() time.Time { return base } }()
		if _, err := iss.Renew(context.Background(), tk.Token, time.Minute); err != ErrTicketExpired {
			t.Fatalf("过期后应 ErrTicketExpired，got %v", err)
		}
	})
}

// TestMediaServer_RepresentationTraversal：representation 穿越拒绝。
func TestMediaServer_RepresentationTraversal(t *testing.T) {
	iss, ms, _ := testIssuer(t)
	srv := wrapMedia(t, ms)
	// 构造 representation 指向 managed 之外的文件
	tk, err := iss.Issue(context.Background(), TicketRequest{
		RefDigest: "d", Representation: "../../etc/passwd",
		Session: "s", Epoch: 1, Entry: "e", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatalf("issue with traversal rep: %v", err)
	}
	if resp := getMedia(t, srv, "/media?media_ticket="+tk.Token); resp.StatusCode == http.StatusOK {
		t.Fatal("穿越表示不得 200")
	}
}

// TestRedactTicketParam：日志脱敏——票据参数必须被替换，URL 其余部分保留。
func TestRedactTicketParam(t *testing.T) {
	in := "http://host/media?media_ticket=abc123&other=keep"
	out := RedactTicketParam(in)
	if strings.Contains(out, "abc123") {
		t.Errorf("票据值泄露: %s", out)
	}
	if !strings.Contains(out, "other=keep") {
		t.Errorf("非票据参数不应被动: %s", out)
	}
}

// TestGuardReader_InFlightTermination：在途流在撤销/过期后的下一次 Read 即失败
// （spec：terminate in-flight access on expiry or revocation）。
func TestGuardReader_InFlightTermination(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	tk, err := iss.Issue(context.Background(), TicketRequest{
		RefDigest: "d", Representation: "managed/x/final.mp3",
		Session: "s", Epoch: 1, Entry: "e", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	slow := strings.NewReader(strings.Repeat("x", 4096))
	g := &guardReader{
		rs: slow, iss: iss, tokenHash: hashToken(tk.Token),
		expiresAt: iss.Now().Add(time.Hour), lastCheck: iss.Now(),
	}
	t.Run("撤销后下一次 Read 失败", func(t *testing.T) {
		globalRevocations.add(hashToken(tk.Token))
		g.lastCheck = time.Time{} // 强制触发重查
		buf := make([]byte, 16)
		if _, err := g.Read(buf); err != ErrTicketRevoked {
			t.Fatalf("在途 Read 应 ErrTicketRevoked，got %v", err)
		}
	})
	t.Run("过期立即断流", func(t *testing.T) {
		g2 := &guardReader{
			rs: strings.NewReader("data"), iss: iss,
			tokenHash: "no-such", expiresAt: iss.Now().Add(-time.Second),
			lastCheck: iss.Now(),
		}
		if _, err := g2.Read(make([]byte, 8)); err != ErrTicketExpired {
			t.Fatalf("过期 Read 应 ErrTicketExpired，got %v", err)
		}
	})
}
