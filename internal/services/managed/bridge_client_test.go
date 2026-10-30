package managed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestBridgeClient_Contract：create/enqueue/command/snapshot 的路径、鉴权头与请求体，
// 以及非 2xx 必须带可见错误（不能吞成成功）。
func TestBridgeClient_Contract(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-music" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401}`))
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/music/sessions":
			_, _ = w.Write([]byte(`{"code":200,"data":{"session_id":"ms-1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"code":200,"data":{"receipt_id":"r-1","state":"accepted"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"code":200,"data":{"phase":"playing","current_index":0}}`))
		default:
			_, _ = w.Write([]byte(`{"code":200,"data":{"entry_id":"qe-1"}}`))
		}
	}))
	defer srv.Close()

	client := NewBridgeClient(srv.URL, "tok-music", "")
	if !client.Enabled() {
		t.Fatal("client with base+token must be enabled")
	}
	ctx := context.Background()
	sid, err := client.CreateSession(ctx)
	if err != nil || sid != "ms-1" {
		t.Fatalf("create: sid=%q err=%v", sid, err)
	}
	err = client.Enqueue(ctx, sid, []BridgeRef{
		{Source: "kuwo", ID: "42"},
		{Source: "migu", ID: "600902", Extra: map[string]any{"copyrightId": "c1"}},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	receipt, err := client.Command(ctx, sid, "play", "idem-1", nil, nil)
	if err != nil || receipt["receipt_id"] != "r-1" {
		t.Fatalf("command: %v %v", receipt, err)
	}
	snap, err := client.Snapshot(ctx, sid)
	if err != nil || snap["phase"] != "playing" {
		t.Fatalf("snapshot: %v %v", snap, err)
	}

	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"POST /api/music/sessions ",
		`"transport":"rpc"`,
		`"player_backend":"mediaplayer"`,
		`"source":"kuwo"`,
		`"id":"42"`,
		`"copyrightId":"c1"`,
		`"idem_key":"idem-1"`,
		"GET /api/music/sessions/ms-1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少请求片段 %q\n实际:\n%s", want, joined)
		}
	}
}

// TestBridgeClient_Errors：非 2xx 与空 session_id 都是错误，不能被当成成功。
func TestBridgeClient_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":503,"msg":"MANAGED_TOKENS 未配置"}`))
	}))
	defer srv.Close()

	client := NewBridgeClient(srv.URL, "tok", "lx06")
	if _, err := client.CreateSession(context.Background()); err == nil {
		t.Fatal("503 必须返回错误")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{}}`))
	}))
	defer empty.Close()
	if _, err := NewBridgeClient(empty.URL, "tok", "lx06").CreateSession(context.Background()); err == nil {
		t.Fatal("空 session_id 必须返回错误")
	}

	body := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer body.Close()
	if _, err := NewBridgeClient(body.URL, "tok", "lx06").CreateSession(context.Background()); err == nil {
		t.Fatal("非 JSON 响应必须返回错误")
	}

	if NewBridgeClient("", "tok", "").Enabled() {
		t.Fatal("缺 base_url 时必须 disabled")
	}
	if NewBridgeClient("http://x", "", "").Enabled() {
		t.Fatal("缺 token 时必须 disabled")
	}
	var nilClient *BridgeClient
	if nilClient.Enabled() {
		t.Fatal("nil client 必须 disabled")
	}
}
