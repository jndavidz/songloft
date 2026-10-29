// 真实接线测试（S-02）：AuthMiddleware → 路由处理器，验证插件票据的
// scope 判定、插件自路径归属、管理面封禁、撤销/重启/过期与旧票据退役。
package middleware_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"songloft/internal/middleware"
	"songloft/internal/models"
	"songloft/internal/plugincred"
	"songloft/internal/services"
	"songloft/internal/services/requestidentity"
)

type scopeHarness struct {
	authSvc *services.AuthService
	configs *database.ConfigRepository
	secret  []byte
	router  http.Handler
}

func newScopeHarness(t *testing.T) *scopeHarness {
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
	t.Cleanup(authSvc.Close)

	secret, err := hex.DecodeString(testJWTSecret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}

	echoIdentity := func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestidentity.FromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"has_identity": ok,
			"kind":         string(id.Kind),
			"client_id":    id.ClientID,
		})
	}

	r := chi.NewRouter()
	r.Use(middleware.AuthMiddleware(authSvc))
	r.Get("/api/v1/songs", echoIdentity)
	r.Post("/api/v1/songs", echoIdentity)
	r.Get("/api/v1/settings/music-path", echoIdentity)
	r.Get("/api/v1/jsplugin/{entryPath}/*", echoIdentity)
	r.Get("/api/v1/jsplugins/", echoIdentity)

	return &scopeHarness{authSvc: authSvc, configs: mdb.ConfigRepository(), secret: secret, router: r}
}

func (h *scopeHarness) issue(t *testing.T, entry string, scopes ...string) string {
	t.Helper()
	cred, err := h.authSvc.IssuePluginCredential(context.Background(), entry, scopes, time.Hour)
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}
	return cred.Token
}

func (h *scopeHarness) do(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return h.doFrom(t, "192.0.2.1:1234", method, path, token)
}

func (h *scopeHarness) doFrom(t *testing.T, remoteAddr, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestPluginCredentialScopeWiring(t *testing.T) {
	h := newScopeHarness(t)

	readOnly := h.issue(t, "demo-plugin", plugincred.ScopeSongsRead)

	t.Run("read scope allows GET songs", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/songs", readOnly)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["kind"] != "user" || body["client_id"] != "plugin:demo-plugin" {
			t.Fatalf("identity = %v", body)
		}
	})

	t.Run("missing write scope denies POST songs", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/songs", readOnly)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("write scope allows POST songs", func(t *testing.T) {
		token := h.issue(t, "writer", plugincred.ScopeSongsWrite)
		rec := h.do(t, http.MethodPost, "/api/v1/songs", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("management and settings are unreachable", func(t *testing.T) {
		for _, path := range []string{"/api/v1/settings/music-path", "/api/v1/jsplugins/"} {
			rec := h.do(t, http.MethodGet, path, readOnly)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403", path, rec.Code)
			}
		}
	})

	t.Run("plugin can only reach its own subpath", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/jsplugin/demo-plugin/api/ping", readOnly)
		if rec.Code != http.StatusOK {
			t.Fatalf("own path status = %d, body = %s", rec.Code, rec.Body.String())
		}
		rec = h.do(t, http.MethodGet, "/api/v1/jsplugin/other-plugin/api/ping", readOnly)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-plugin status = %d, want 403", rec.Code)
		}
	})
}

func TestPluginCredentialRevokedAndExpired(t *testing.T) {
	h := newScopeHarness(t)

	token := h.issue(t, "demo-plugin", plugincred.ScopeSongsRead)
	if rec := h.do(t, http.MethodGet, "/api/v1/songs", token); rec.Code != http.StatusOK {
		t.Fatalf("initial status = %d", rec.Code)
	}

	if err := h.authSvc.RevokePluginCredentials(context.Background(), "demo-plugin"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/songs", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d, want 401", rec.Code)
	}

	shortLived, err := h.authSvc.IssuePluginCredential(context.Background(), "demo-plugin", []string{plugincred.ScopeSongsRead}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("issue short-lived: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if rec := h.do(t, http.MethodGet, "/api/v1/songs", shortLived.Token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired status = %d, want 401", rec.Code)
	}
}

func TestLegacyPluginSystemTokenWindowAtWiring(t *testing.T) {
	h := newScopeHarness(t)

	claims := &services.Claims{
		ClientID: "plugin-system",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(100 * 365 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        "legacy-token",
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(h.secret)
	if err != nil {
		t.Fatalf("sign legacy: %v", err)
	}

	// 无窗口：直接拒绝。
	if rec := h.do(t, http.MethodGet, "/api/v1/songs", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy without window status = %d, want 401", rec.Code)
	}

	// 窗口内：仅允许插件数据面；管理面与插件自身路径仍然拒绝。
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := h.configs.Set(context.Background(), &models.Config{Key: plugincred.LegacyUntilConfigKey, Value: until}); err != nil {
		t.Fatalf("seed window: %v", err)
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/songs", token); rec.Code != http.StatusOK {
		t.Fatalf("legacy songs status = %d, want 200", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/settings/music-path", token); rec.Code != http.StatusForbidden {
		t.Fatalf("legacy settings status = %d, want 403", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/jsplugin/demo-plugin/api/ping", token); rec.Code != http.StatusOK {
		t.Fatalf("legacy self path status = %d, want 200 (window preserves old behaviour)", rec.Code)
	}
}

func TestSystemTokenWiring(t *testing.T) {
	h := newScopeHarness(t)
	token, _, err := h.authSvc.IssueSystemToken(time.Hour)
	if err != nil {
		t.Fatalf("issue system token: %v", err)
	}

	rec := h.doFrom(t, "127.0.0.1:1234", http.MethodGet, "/api/v1/songs", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["kind"] != "system" {
		t.Fatalf("identity = %v, want system", body)
	}

	// 系统票据只限本机调用：外部来源必须拒绝。
	if rec := h.do(t, http.MethodGet, "/api/v1/songs", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-loopback system token status = %d, want 401", rec.Code)
	}
}

func TestUserTokenStillHasFullAccess(t *testing.T) {
	h := newScopeHarness(t)
	login, err := h.authSvc.Login(context.Background(), "admin", "password", "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	for _, path := range []string{"/api/v1/settings/music-path", "/api/v1/songs"} {
		rec := h.do(t, http.MethodGet, path, login.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
	}
}
