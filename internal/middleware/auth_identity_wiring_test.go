// 真实接线测试（S-01）：AuthMiddleware → handler → playactivity.Registry。
//
// 缺陷取证：middleware 用类型化私有 key 写 client_id，playactivity.SessionFromContext
// 用裸 string key 读——两个 key 类型不同永远取不到值，所有认证请求都落进共享零值桶，
// 两个真实客户端会互相取消对方的工作。测试必须经过真实 middleware→handler→Registry
// 接线，不允许再自造字符串 context key。
package middleware_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"songloft/internal/database/testutil"
	"songloft/internal/middleware"
	"songloft/internal/models"
	"songloft/internal/services"
	"songloft/internal/services/playactivity"
	"songloft/internal/services/requestidentity"
)

const testJWTSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// trackedWork 是 handler 注册进 registry 后暂存的工作，供测试断言其 ctx 存活。
type trackedWork struct {
	ctx     context.Context
	release func()
}

// harness 用真实 AuthService + 真实 AuthMiddleware + 真实 Registry 搭一条完整链路。
// handler 的 Track/Activate 行为与 SongHandler 生产路径一致（SessionFromContext(r.Context())）。
type harness struct {
	authSvc *services.AuthService
	reg     *playactivity.Registry
	srv     *httptest.Server // 业务路由：挂真实 AuthMiddleware
	sysSrv  *httptest.Server // 内部工作路由：无鉴权中间件（系统任务的真实拓扑）
	mu      sync.Mutex
	works   map[string]trackedWork
	seq     int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mdb := testutil.OpenMemoryDB(t)
	if err := mdb.ConfigRepository().Set(context.Background(),
		&models.Config{Key: "jwt_secret", Value: testJWTSecret}); err != nil {
		t.Fatalf("seed jwt_secret: %v", err)
	}
	authSvc, err := services.NewAuthService(mdb.ConfigRepository(), mdb.TokenRepository(), "admin", "password")
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	h := &harness{authSvc: authSvc, reg: playactivity.New(), works: make(map[string]trackedWork)}

	r := chi.NewRouter()
	r.Use(middleware.AuthMiddleware(authSvc))
	r.Post("/work/{song}", func(w http.ResponseWriter, req *http.Request) {
		songID, err := strconv.ParseInt(chi.URLParam(req, "song"), 10, 64)
		if err != nil {
			http.Error(w, "bad song", http.StatusBadRequest)
			return
		}
		cat := playactivity.CatPlay
		switch req.URL.Query().Get("cat") {
		case "transcode":
			cat = playactivity.CatTranscode
		case "prefetch":
			cat = playactivity.CatPrefetch
		}
		sk := playactivity.SessionFromContext(req.Context())
		ctx, release := h.reg.Track(context.Background(), sk, songID, cat)
		h.mu.Lock()
		h.seq++
		id := "w" + strconv.Itoa(h.seq)
		h.works[id] = trackedWork{ctx: ctx, release: release}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	r.Post("/activate/{song}", func(w http.ResponseWriter, req *http.Request) {
		songID, err := strconv.ParseInt(chi.URLParam(req, "song"), 10, 64)
		if err != nil {
			http.Error(w, "bad song", http.StatusBadRequest)
			return
		}
		h.reg.Activate(playactivity.SessionFromContext(req.Context()), songID)
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/whoami", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": middleware.ClientIDFromContext(req.Context())})
	})

	// 系统工作入口挂在无鉴权中间件的独立路由上（后台任务不经过 HTTP 鉴权，
	// 但进入业务层前必须显式声明 system identity 并 Track）。
	sys := chi.NewRouter()
	sys.Post("/system-work/{song}", func(w http.ResponseWriter, req *http.Request) {
		songID, err := strconv.ParseInt(chi.URLParam(req, "song"), 10, 64)
		if err != nil {
			http.Error(w, "bad song", http.StatusBadRequest)
			return
		}
		sk := playactivity.SessionFromContext(requestidentity.SystemContext(req.Context()))
		ctx, release := h.reg.Track(context.Background(), sk, songID, playactivity.CatTranscode)
		h.mu.Lock()
		h.seq++
		id := "w" + strconv.Itoa(h.seq)
		h.works[id] = trackedWork{ctx: ctx, release: release}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})

	h.srv = httptest.NewServer(r)
	h.sysSrv = httptest.NewServer(sys)
	t.Cleanup(func() {
		h.srv.Close()
		h.sysSrv.Close()
	})
	return h
}

func (h *harness) login(t *testing.T) string {
	t.Helper()
	resp, err := h.authSvc.Login(context.Background(), "admin", "password", "wiring-test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return resp.AccessToken
}

func (h *harness) do(t *testing.T, token, method, path string) (*http.Response, string) {
	return h.doURL(t, h.srv.URL, token, method, path)
}

func (h *harness) doURL(t *testing.T, base, token, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp, string(body)
}

func (h *harness) work(t *testing.T, token, path string) string {
	return h.workOn(t, h.srv.URL, token, path)
}

func (h *harness) workOn(t *testing.T, base, token, path string) string {
	t.Helper()
	resp, body := h.doURL(t, base, token, "POST", path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s", path, resp.StatusCode, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.ID == "" {
		t.Fatalf("POST %s: bad body %q (err=%v)", path, body, err)
	}
	return out.ID
}

func (h *harness) canceled(id string) bool {
	h.mu.Lock()
	w := h.works[id]
	h.mu.Unlock()
	return w.ctx.Err() != nil
}

func (h *harness) releaseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.works {
		w.release()
	}
}

// waitCanceled 短轮询等 cancel 可见（Activate 内 cancel 同步触发，ctx.Err 可见性经 close 完成）。
func waitCanceled(h *harness, id string) bool {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.canceled(id) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return h.canceled(id)
}

// TestWiring_TwoAuthenticatedClientsDoNotCancelEachOther 是 S-01 的缺陷证明用例：
// 修复前两个客户端都落进共享零值桶，B 切歌会取消 A 的工作。
func TestWiring_TwoAuthenticatedClientsDoNotCancelEachOther(t *testing.T) {
	h := newHarness(t)
	defer h.releaseAll()
	tokenA, tokenB := h.login(t), h.login(t)

	workA := h.work(t, tokenA, "/work/100")
	workB := h.work(t, tokenB, "/work/200")

	// B 切到自己的 song 200
	h.do(t, tokenB, "POST", "/activate/200")

	if waitCanceled(h, workA) {
		t.Errorf("客户端 B 的 Activate 不应取消客户端 A 的 song 100 工作（跨会话隔离）")
	}
	if h.canceled(workB) {
		t.Errorf("B 自己的当前歌 song 200 工作不应被 cancel")
	}
}

// TestWiring_SameSessionOldSongStillCanceled 锁定既有业务语义：同会话切歌仍取消旧工作。
func TestWiring_SameSessionOldSongStillCanceled(t *testing.T) {
	h := newHarness(t)
	defer h.releaseAll()
	token := h.login(t)

	old := h.work(t, token, "/work/100")
	cur := h.work(t, token, "/work/101")

	h.do(t, token, "POST", "/activate/101")

	if !waitCanceled(h, old) {
		t.Errorf("同会话旧歌 song 100 的工作应被 cancel")
	}
	if h.canceled(cur) {
		t.Errorf("当前歌 song 101 的工作不应被 cancel")
	}
}

// TestWiring_PrefetchSurvivesActivate 锁定 songloft#300 语义：Activate 不取消 prefetch。
func TestWiring_PrefetchSurvivesActivate(t *testing.T) {
	h := newHarness(t)
	defer h.releaseAll()
	token := h.login(t)

	playing := h.work(t, token, "/work/100")
	prefetch := h.work(t, token, "/work/101?cat=prefetch")

	h.do(t, token, "POST", "/activate/100")

	if h.canceled(prefetch) {
		t.Errorf("下一首 song 101 的 prefetch 不应被 cancel（songloft#300）")
	}
	if h.canceled(playing) {
		t.Errorf("当前歌 song 100 的 play 不应被 cancel")
	}
}

// TestWiring_MissingSubjectTokenFailsClosed：真实认证请求缺主体时必须失败关闭，
// 不得建立任何 registry 工作（更不得落进共享零值桶）。
func TestWiring_MissingSubjectTokenFailsClosed(t *testing.T) {
	h := newHarness(t)
	defer h.releaseAll()
	h.login(t) // 确保服务可用

	claims := &services.Claims{
		ClientID: "",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	resp, body := h.do(t, signed, "POST", "/work/100")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("缺 client_id 的 token 应 401 失败关闭，got %d body=%s", resp.StatusCode, body)
	}
	if n := h.reg.TotalSize(); n != 0 {
		t.Errorf("缺主体请求不得注册任何工作，registry 总数=%d", n)
	}
}

// TestWiring_SystemWorkIsIsolatedFromUserSessions：
// 显式系统身份的工作落在独立系统桶，任何用户切歌都不取消它。
func TestWiring_SystemWorkIsIsolatedFromUserSessions(t *testing.T) {
	h := newHarness(t)
	defer h.releaseAll()
	token := h.login(t)

	sysWork := h.workOn(t, h.sysSrv.URL, "", "/system-work/100")
	userWork := h.work(t, token, "/work/200")

	// 用户切歌
	h.do(t, token, "POST", "/activate/300")

	if h.canceled(sysWork) {
		t.Errorf("用户 Activate 不得取消显式系统身份的工作")
	}
	if waitCanceled(h, userWork) != true {
		t.Errorf("用户自己的旧歌 song 200 工作应被 cancel")
	}
}

// TestWiring_ClientIDFromContextReturnsAuthenticatedSubject：
// ClientIDFromContext 与 requestidentity 同源，认证后能取到真实主体。
func TestWiring_ClientIDFromContextReturnsAuthenticatedSubject(t *testing.T) {
	h := newHarness(t)
	token := h.login(t)

	resp, body := h.do(t, token, "GET", "/whoami")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami: status=%d body=%s", resp.StatusCode, body)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parse whoami body %q: %v", body, err)
	}
	if out.ClientID == "" {
		t.Errorf("认证请求应能取到非空 client_id")
	}
}
