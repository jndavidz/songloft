// 浏览面接线测试（spec: songloft .scratch/musicfree-style-library 票 06）：
// player 在线浏览走既有 jsplugin catch-all（/api/v1/jsplugin/{entry}/*），本测试验证
// 该链路的鉴权语义 —— 无 JWT 401、跨插件票据 403、本插件票据与用户 JWT 放行；
// server 对浏览载荷零落库（catch-all 原样透传 QuickJS 响应，无 songs 写路径）。
package middleware_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"songloft/internal/database/testutil"
	"songloft/internal/middleware"
	"songloft/internal/models"
	"songloft/internal/services"
	"songloft/internal/services/requestidentity"
)

func newBrowseHarness(t *testing.T) *scopeHarness {
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
	// 与真实注册一致：catch-all HandleFunc 覆盖全部方法（routes.go RegisterAPIRoutes）
	r.HandleFunc("/api/v1/jsplugin/{entryPath}/*", echoIdentity)

	return &scopeHarness{authSvc: authSvc, configs: mdb.ConfigRepository(), secret: secret, router: r}
}

func TestBrowseSurfaceWiring(t *testing.T) {
	h := newBrowseHarness(t)

	browsePaths := []string{
		"/api/v1/jsplugin/kwqq/browse/search",
		"/api/v1/jsplugin/kwqq/album/info",
		"/api/v1/jsplugin/kwqq/artist/works",
		"/api/v1/jsplugin/kwqq/sheet/info",
		"/api/v1/jsplugin/kwqq/toplists",
		"/api/v1/jsplugin/kwqq/recommend/tags",
		"/api/v1/jsplugin/kwqq/recommend/sheets",
		"/api/v1/jsplugin/kwqq/music/comments",
	}

	t.Run("no JWT is 401 on every browse path", func(t *testing.T) {
		for _, path := range browsePaths {
			if rec := h.do(t, http.MethodPost, path, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d, want 401", path, rec.Code)
			}
		}
	})

	t.Run("cross-plugin token is 403", func(t *testing.T) {
		other := h.issue(t, "other-plugin", "net")
		for _, path := range browsePaths {
			if rec := h.do(t, http.MethodPost, path, other); rec.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403 (plugin tokens confined to own entry)", path, rec.Code)
			}
		}
	})

	t.Run("own plugin token passes auth", func(t *testing.T) {
		own := h.issue(t, "kwqq", "net", "storage")
		for _, path := range browsePaths {
			if rec := h.do(t, http.MethodPost, path, own); rec.Code != http.StatusOK {
				t.Fatalf("%s status = %d, body = %s", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("user JWT passes auth", func(t *testing.T) {
		login, err := h.authSvc.Login(context.Background(), "admin", "password", "test")
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		if rec := h.do(t, http.MethodPost, browsePaths[0], login.AccessToken); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}
