package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingResolver 统计 resolve 次数——no-prefetch 不变量的计量器。
type countingResolver struct {
	mu    sync.Mutex
	calls []ResolveRequest
	next  func(req ResolveRequest) (*ResolveResult, error)
}

func (c *countingResolver) Resolve(ctx context.Context, req ResolveRequest) (*ResolveResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, req)
	n := c.next
	c.mu.Unlock()
	return n(req)
}

func (c *countingResolver) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func kwqqRef(id string) *Ref {
	ref, _ := MakeRef("kwqq", id, nil, []string{"kwqq", "migu", "bilibili"})
	return ref
}

func okResult(t *testing.T, requiresTranscode bool) *ResolveResult {
	t.Helper()
	codec, ext := "mp3", "mp3"
	if requiresTranscode {
		codec, ext = "aac", "m4a"
	}
	return &ResolveResult{
		TrackRef: kwqqRef("12345"), ResolvedSource: "kwqq", RequestedQuality: "high",
		DeliveredQuality: strPtr("128k"), Codec: codec, URL: "http://media.example/audio",
		Ext: ext, SizeBytes: 0, Headers: map[string]string{},
		Verification: Verification{Level: "none", RequiresByteVerification: true,
			RequiresTranscode: requiresTranscode},
		Ready: true, Verified: false,
	}
}

func strPtr(s string) *string { return &s }

// fakeDownloader 把 fixture 复制进 destDir 模拟完整下载。
func fakeDownloader(fixture string) func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error) {
	return func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error) {
		// 按解析结果声明的 ext 选 fixture——上游给什么就是什么，
		// 不因 requires_transcode 标记而变（codec 与标记可以撒谎）。
		name := "valid_128k.mp3"
		if result.Ext == "m4a" {
			name = "audio.m4a"
		}
		src, err := os.Open(filepath.Join("testdata", name))
		if err != nil {
			return "", 0, err
		}
		defer src.Close()
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return "", 0, err
		}
		dstPath := filepath.Join(destDir, "source."+result.Ext)
		dst, err := os.Create(dstPath)
		if err != nil {
			return "", 0, err
		}
		defer dst.Close()
		n, err := io.Copy(dst, src)
		if err != nil {
			return "", 0, err
		}
		return dstPath, n, nil
	}
}

// fakeTranscoder 模拟 AAC→MP3：产出真实可验证的 MP3。
func fakeTranscoder(ctx context.Context, inputPath, destDir string) (string, error) {
	src, err := os.Open(filepath.Join("testdata", "valid_128k.mp3"))
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(destDir, "final.mp3"))
	if err != nil {
		return "", err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return filepath.Join(destDir, "final.mp3"), err
}

func testDeps(t *testing.T, res *countingResolver) PrepareDeps {
	t.Helper()
	return PrepareDeps{
		Resolver:   res,
		Downloader: fakeDownloader(""),
		Transcoder: fakeTranscoder,
		WorkDir:    t.TempDir(),
	}
}

// TestPrepare_OnlyCurrentEntryResolves 是 no-prefetch 不变量的核心：
// 导入/建队（不调 StartPrepare）resolve=0；选中当前项只 resolve 一次。
func TestPrepare_OnlyCurrentEntryResolves(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res))

	// 模拟 20 项导入/建队：只登记身份，不发起任何准备
	for i := 0; i < 20; i++ {
		if _, err := MakeRef("kwqq", fmt.Sprintf("song-%d", i), nil, nil); err != nil {
			t.Fatalf("import ref: %v", err)
		}
	}
	if res.count() != 0 {
		t.Fatalf("导入 20 项后 resolve 次数应为 0，got %d", res.count())
	}

	// 选中当前项 → 只有当前项解析一次
	op, err := m.StartPrepare("songloft-managed", OpKey{Session: "sess-1", Epoch: 1, Entry: "e1"}, kwqqRef("12345"))
	if err != nil {
		t.Fatalf("StartPrepare: %v", err)
	}
	select {
	case <-op.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("prepare 未在时限内完成")
	}
	if res.count() != 1 {
		t.Fatalf("选中后应恰好 resolve 1 次，got %d", res.count())
	}
	q, ok := m.Query("songloft-managed", OpKey{Session: "sess-1", Epoch: 1, Entry: "e1"})
	if !ok || q.State != StateReady {
		t.Fatalf("当前项应 ready，got %+v ok=%v", q, ok)
	}
}

// TestPrepare_LateReadyNotDeliverable：换代后旧代次的完成不可投送。
func TestPrepare_LateReadyNotDeliverable(t *testing.T) {
	gate := make(chan struct{})
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		<-gate // 卡住管线，制造「完成前换代」窗口
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res))

	oldKey := OpKey{Session: "sess-1", Epoch: 1, Entry: "e1"}
	op, err := m.StartPrepare("issuer-a", oldKey, kwqqRef("1"))
	if err != nil {
		t.Fatalf("StartPrepare: %v", err)
	}

	// 同 entry 新代次
	newKey := OpKey{Session: "sess-1", Epoch: 2, Entry: "e1"}
	newOp, err := m.StartPrepare("issuer-a", newKey, kwqqRef("1"))
	if err != nil {
		t.Fatalf("StartPrepare new epoch: %v", err)
	}
	close(gate) // 放行旧代次管线——它的 resolve 已返回，但代次已换代

	select {
	case <-op.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("旧 op 应立即终态")
	}
	// 旧代次 op 已从表里移除；直接断言持有句柄的终态
	if op.State != StateCanceled {
		t.Fatalf("换代后旧代次应 canceled（迟到 ready 不可投送），got %s", op.State)
	}
	// 等新代次管线收尾，避免它的下载写 WorkDir 与 TempDir 清理竞态
	// （Windows unlinkat "directory is not empty"）。
	select {
	case <-newOp.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("新代次 op 应正常完成")
	}
}

// TestPrepare_SameEpochDuplicateRejected：同代次重复发起被拒。
func TestPrepare_SameEpochDuplicateRejected(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		time.Sleep(50 * time.Millisecond)
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res))
	key := OpKey{Session: "s", Epoch: 1, Entry: "e"}
	if _, err := m.StartPrepare("i", key, kwqqRef("1")); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := m.StartPrepare("i", key, kwqqRef("1")); err == nil {
		t.Fatal("同 (session,epoch,entry) 重复发起应被拒")
	}
}

// TestPrepare_CrossIssuerCancelDenied：跨主体取消拒绝，不泄露存在性。
func TestPrepare_CrossIssuerCancelDenied(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		time.Sleep(80 * time.Millisecond)
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res))
	key := OpKey{Session: "s", Epoch: 1, Entry: "e"}
	if _, err := m.StartPrepare("issuer-a", key, kwqqRef("1")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Cancel("issuer-b", key); err == nil || !strings.Contains(err.Error(), "no op") {
		t.Fatalf("跨主体取消应按不存在拒绝，got %v", err)
	}
	if err := m.Cancel("issuer-a", key); err != nil {
		t.Fatalf("本人取消应成功: %v", err)
	}
	q, _ := m.Query("issuer-a", key)
	if q.State != StateCanceled {
		t.Fatalf("取消后应 canceled，got %s", q.State)
	}
}

// TestPrepare_AACRequiresTranscode：AAC 规范化输入只经转码路径，产出 MP3 后 ready；
// 转码不可用时保持 failed，绝不把 AAC 直投为 ready。
func TestPrepare_AACRequiresTranscode(t *testing.T) {
	t.Run("AAC 经转码 → ready 且终品是 MP3", func(t *testing.T) {
		res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
			return okResult(t, true), nil // requires_transcode
		}}
		m := NewManager(testDeps(t, res))
		op, err := m.StartPrepare("i", OpKey{Session: "s", Epoch: 1, Entry: "e"}, kwqqRef("1"))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		select {
		case <-op.Done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
		q, _ := m.Query("i", OpKey{Session: "s", Epoch: 1, Entry: "e"})
		if q.State != StateReady {
			t.Fatalf("AAC 规范化后应 ready，state=%s err=%s", q.State, q.Error)
		}
	})
	t.Run("转码失败 → failed，不原码兜底", func(t *testing.T) {
		res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
			return okResult(t, true), nil
		}}
		deps := testDeps(t, res)
		deps.Transcoder = func(ctx context.Context, inputPath, destDir string) (string, error) {
			return "", fmt.Errorf("transcoder unavailable")
		}
		m := NewManager(deps)
		op, _ := m.StartPrepare("i", OpKey{Session: "s", Epoch: 1, Entry: "e"}, kwqqRef("1"))
		select {
		case <-op.Done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
		q, _ := m.Query("i", OpKey{Session: "s", Epoch: 1, Entry: "e"})
		if q.State != StateFailed {
			t.Fatalf("转码失败应 failed（不原码兜底），state=%s", q.State)
		}
	})
	t.Run("resolver 谎报无需转码的 AAC → 验证拒绝", func(t *testing.T) {
		res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
			r := okResult(t, false) // 谎称 mp3
			r.Verification.RequiresTranscode = false
			return r, nil
		}}
		deps := testDeps(t, res)
		// 下载落盘的是真实 AAC fixture（模拟上游真给了 AAC，而 resolver 谎称 mp3）
		deps.Downloader = func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error) {
			return fakeDownloader("")(ctx, &ResolveResult{Ext: "m4a", Verification: Verification{RequiresTranscode: false}}, destDir)
		}
		m := NewManager(deps)
		op, _ := m.StartPrepare("i", OpKey{Session: "s", Epoch: 1, Entry: "e"}, kwqqRef("1"))
		select {
		case <-op.Done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
		q, _ := m.Query("i", OpKey{Session: "s", Epoch: 1, Entry: "e"})
		if q.State != StateFailed {
			t.Fatalf("AAC 直投 ready 必须被验证拒绝，state=%s", q.State)
		}
	})
}

// TestPrepare_RestoreInterruptedOnRestart：重启后未完成操作进入 interrupted，
// 不自动续跑（恢复语义，不是自动播放）。
func TestPrepare_RestoreInterruptedOnRestart(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		<-make(chan struct{}) // 永久阻塞，模拟进行中
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res))
	key := OpKey{Session: "s", Epoch: 1, Entry: "e"}
	if _, err := m.StartPrepare("i", key, kwqqRef("1")); err != nil {
		t.Fatalf("start: %v", err)
	}
	m.MarkAllInterruptedOnRestart()
	q, _ := m.Query("i", key)
	if q.State != StateInterrupted {
		t.Fatalf("重启后应 interrupted，got %s", q.State)
	}
	if m.ActiveCount() != 0 {
		t.Fatalf("interrupted 不应计入活跃，got %d", m.ActiveCount())
	}
}

// TestPrepare_RequiresIssuer：无 issuer 的操作拒绝（系统工作也必须显式身份）。
func TestPrepare_RequiresIssuer(t *testing.T) {
	m := NewManager(PrepareDeps{WorkDir: t.TempDir()})
	if _, err := m.StartPrepare("", OpKey{Session: "s", Epoch: 1, Entry: "e"}, kwqqRef("1")); err == nil {
		t.Fatal("缺 issuer 应拒绝")
	}
}

// TestKwqqClient_Envelope：客户端解析 envelope、业务码与 scope 语义。
func TestKwqqClient_Envelope(t *testing.T) {
	t.Run("成功 envelope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/playback/resolve" {
				t.Errorf("path=%s", r.URL.Path)
			}
			if r.Header.Get("X-Service-Token") != "tok-prepare" {
				t.Errorf("token header=%q", r.Header.Get("X-Service-Token"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			tr := body["track_ref"].(map[string]any)
			if tr["source"] != "kwqq" {
				t.Errorf("source=%v", tr["source"])
			}
			fmt.Fprintf(w, `{"code":200,"msg":"success","data":{"track_ref":{"source":"kwqq","id":"1","extra":{}},"url":"http://x/a.mp3","codec":"mp3","verified":false,"ready":true,"verification":{"level":"none","requires_byte_verification":true,"requires_transcode":false}},"timestamp":1}`)
		}))
		defer srv.Close()
		c := &KwqqClient{BaseURL: srv.URL, Token: "tok-prepare"}
		out, err := c.Resolve(context.Background(), ResolveRequest{Ref: kwqqRef("1"), Target: "device"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if out.URL != "http://x/a.mp3" || !out.Ready {
			t.Errorf("unexpected result: %+v", out)
		}
	})
	t.Run("HTTP 200 + code 403 是失败（envelope 契约）", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":403,"msg":"scope denied","data":null,"timestamp":1}`)
		}))
		defer srv.Close()
		c := &KwqqClient{BaseURL: srv.URL, Token: "tok"}
		_, err := c.Resolve(context.Background(), ResolveRequest{Ref: kwqqRef("1")})
		var re *ResolveError
		if err == nil || !errors.As(err, &re) || re.Code != 403 {
			t.Fatalf("应得到 ResolveError code=403，got %v", err)
		}
	})
	t.Run("上游宣称 verified=true 按契约拒绝", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":200,"msg":"success","data":{"url":"http://x","verified":true},"timestamp":1}`)
		}))
		defer srv.Close()
		c := &KwqqClient{BaseURL: srv.URL, Token: "tok"}
		_, err := c.Resolve(context.Background(), ResolveRequest{Ref: kwqqRef("1")})
		if err == nil || !strings.Contains(err.Error(), "verified=true") {
			t.Fatalf("上游 verified=true 应被拒绝，got %v", err)
		}
	})
}

// TestPrepare_RapidGenerationChangeStress：快速换代压力（review 发现的
// double-close 竞态窗口：旧 op 已终态但 defer 尚未清 current 时换代）。
// 若 startFinish 终态守卫缺失，本测试会以 close of closed channel panic。
func TestPrepare_RapidGenerationChangeStress(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		// 立即失败：直接命中 fail→close 与 defer 清理之间的换代窗口，
		// 且不产生下载产物（避免与 TempDir 清理竞态）
		return nil, fmt.Errorf("instant resolver failure")
	}}
	m := NewManager(testDeps(t, res))
	for i := int64(1); i <= 300; i++ {
		key := OpKey{Session: "s", Epoch: i, Entry: "e"}
		if _, err := m.StartPrepare("i", key, kwqqRef("1")); err != nil {
			t.Fatalf("epoch %d: %v", i, err)
		}
		// 不等待完成立即换代—— maximize 竞态窗口
	}
}
