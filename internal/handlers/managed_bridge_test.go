package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"songloft/internal/services/managed"
)

// fakeBridge 记录 BFF 转发到 bridge 的请求，并返回最小合法响应。
type fakeBridge struct {
	mu     sync.Mutex
	calls  []string
	queued int
}

func (f *fakeBridge) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+string(raw))
		f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/queue"):
			f.mu.Lock()
			f.queued++
			n := f.queued
			f.mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":200,"data":{"entry_id":"e-b%d"}}`, n)))
		case strings.HasSuffix(r.URL.Path, "/commands"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"code":200,"data":{"receipt_id":"r-1","state":"accepted"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/music/sessions":
			_, _ = w.Write([]byte(`{"code":200,"data":{"session_id":"ms-9"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":200,"data":{}}`))
		}
	})
}

func (f *fakeBridge) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func newBridgeHandler(t *testing.T) (*ManagedHandler, *fakeBridge) {
	t.Helper()
	fb := &fakeBridge{}
	srv := httptest.NewServer(fb.handler())
	t.Cleanup(srv.Close)
	h := NewManagedHandler(managed.NewManagedBFF(nil), nil, nil, nil, nil, nil)
	h.SetBridgeClient(managed.NewBridgeClient(srv.URL, "tok", "lx06"))
	return h, fb
}

func postJSON(t *testing.T, fn http.HandlerFunc, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// TestManagedHandler_BridgeHandoffForward：两段式 handoff 确认后，
// 整队（只含身份）与 play 必须转发给 bridge；未确认前不得转发。
func TestManagedHandler_BridgeHandoffForward(t *testing.T) {
	h, fb := newBridgeHandler(t)
	rec := postJSON(t, h.SetQueue, `{"session":"app-1","epoch":1,"entries":[
		{"entry":"e1","ref":{"source":"kuwo","id":"42"}},
		{"entry":"e2","ref":{"source":"migu","id":"600902","extra":{"copyrightId":"c1"}}}]}`)
	if rec.Code != 200 {
		t.Fatalf("set queue: %d %s", rec.Code, rec.Body.String())
	}
	if got := fb.joined(); got != "" {
		t.Fatalf("handoff 确认前不应触达 bridge: %s", got)
	}

	rec = postJSON(t, h.RequestHandoff, `{"session":"app-1"}`)
	if rec.Code != 200 {
		t.Fatalf("request handoff: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, h.ConfirmHandoffStop, `{"session":"app-1","epoch":1}`)
	if rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	got := fb.joined()
	for _, want := range []string{
		"POST /api/music/sessions ",
		`"source":"kuwo"`,
		`"copyrightId":"c1"`,
		`"kind":"play"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bridge 未收到 %q\n实际:\n%s", want, got)
		}
	}
}

// TestManagedHandler_BridgeCommandForward：managed 阶段命令转发且幂等键绑定代次。
func TestManagedHandler_BridgeCommandForward(t *testing.T) {
	h, fb := newBridgeHandler(t)
	postJSON(t, h.SetQueue, `{"session":"app-2","epoch":1,"entries":[{"entry":"e1","ref":{"source":"kuwo","id":"42"}}]}`)
	postJSON(t, h.RequestHandoff, `{"session":"app-2"}`)
	if rec := postJSON(t, h.ConfirmHandoffStop, `{"session":"app-2","epoch":1}`); rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	rec := postJSON(t, h.Execute, `{"session":"app-2","name":"next","epoch":1,"entry":"e2"}`)
	if rec.Code != 200 {
		t.Fatalf("execute next: %d %s", rec.Code, rec.Body.String())
	}
	got := fb.joined()
	if !strings.Contains(got, `"kind":"next"`) {
		t.Errorf("next 未转发: %s", got)
	}
	if !strings.Contains(got, "songloft-app-2-1-next-e2") {
		t.Errorf("幂等键应绑定 session/epoch/命令/entry: %s", got)
	}
}

// TestManagedHandler_EntryTranslation：手机侧 entry（e1/e2）必须翻译成 bridge 的
// entry_id（e-b1/e-b2）再转发，否则 bridge 会以 unknown queue entry 拒绝。
func TestManagedHandler_EntryTranslation(t *testing.T) {
	h, fb := newBridgeHandler(t)
	postJSON(t, h.SetQueue, `{"session":"app-4","epoch":1,"entries":[
		{"entry":"e1","ref":{"source":"kuwo","id":"42"}},
		{"entry":"e2","ref":{"source":"kuwo","id":"43"}}]}`)
	postJSON(t, h.RequestHandoff, `{"session":"app-4"}`)
	if rec := postJSON(t, h.ConfirmHandoffStop, `{"session":"app-4","epoch":1}`); rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	rec := postJSON(t, h.Execute, `{"session":"app-4","name":"jump","epoch":1,"entry":"e2"}`)
	if rec.Code != 200 {
		t.Fatalf("execute jump: %d %s", rec.Code, rec.Body.String())
	}
	got := fb.joined()
	if !strings.Contains(got, `"entry_id":"e-b2"`) {
		t.Errorf("jump 未翻译成 bridge entry_id: %s", got)
	}
	if strings.Contains(got, `"entry_id":"e2"`) {
		t.Errorf("不得把手机侧 entry 原样转发: %s", got)
	}
}

// TestManagedHandler_ConfirmIsIdempotent：handoff 确认重复调用应 200（手机重试不 409）。
func TestManagedHandler_ConfirmIsIdempotent(t *testing.T) {
	h, _ := newBridgeHandler(t)
	postJSON(t, h.SetQueue, `{"session":"app-5","epoch":1,"entries":[{"entry":"e1","ref":{"source":"kuwo","id":"42"}}]}`)
	postJSON(t, h.RequestHandoff, `{"session":"app-5"}`)
	if rec := postJSON(t, h.ConfirmHandoffStop, `{"session":"app-5","epoch":1}`); rec.Code != 200 {
		t.Fatalf("first confirm: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, h.ConfirmHandoffStop, `{"session":"app-5","epoch":1}`); rec.Code != 200 {
		t.Fatalf("retry confirm: %d %s", rec.Code, rec.Body.String())
	}
	// 旧代次的 confirm 不得借「已 managed」放行（会顶掉当前代次的播放）
	if rec := postJSON(t, h.ConfirmHandoffStop, `{"session":"app-5","epoch":0}`); rec.Code != 409 {
		t.Fatalf("stale-epoch confirm 应 409，got %d %s", rec.Code, rec.Body.String())
	}
}

// TestManagedHandler_NoBridgeConfigured：未配置时不报错，BFF 行为不变。
func TestManagedHandler_NoBridgeConfigured(t *testing.T) {
	h := NewManagedHandler(managed.NewManagedBFF(nil), nil, nil, nil, nil, nil)
	if rec := postJSON(t, h.SetQueue, `{"session":"app-3","epoch":1,"entries":[{"entry":"e1","ref":{"source":"kuwo","id":"42"}}]}`); rec.Code != 200 {
		t.Fatalf("set queue without bridge: %d %s", rec.Code, rec.Body.String())
	}
	postJSON(t, h.RequestHandoff, `{"session":"app-3"}`)
	if rec := postJSON(t, h.ConfirmHandoffStop, `{"session":"app-3","epoch":1}`); rec.Code != 200 {
		t.Fatalf("confirm without bridge: %d %s", rec.Code, rec.Body.String())
	}
}
