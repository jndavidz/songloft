package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"songloft/internal/database/testutil"
	"songloft/internal/jsplugin"
	"songloft/internal/models"
	"songloft/internal/services"
)

func newCredentialHandlerHarness(t *testing.T) (*JSPluginHandler, *chi.Mux) {
	t.Helper()
	mdb := testutil.OpenMemoryDB(t)
	if err := mdb.ConfigRepository().Set(context.Background(), &models.Config{
		Key: "jwt_secret", Value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}); err != nil {
		t.Fatalf("seed jwt_secret: %v", err)
	}
	authSvc, err := services.NewAuthService(mdb.ConfigRepository(), mdb.TokenRepository(), "admin", "password")
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(authSvc.Close)

	pluginsDir := t.TempDir()
	dataDir := t.TempDir()
	manager := jsplugin.NewManager(mdb.JSPluginRepository(), pluginsDir, dataDir, "", nil, mdb)
	configs := services.NewConfigService(mdb.ConfigRepository())
	manager.SetAuthService(authSvc, "58091")
	manager.SetConfigService(configs)
	t.Cleanup(func() { manager.Close() })

	handler := NewJSPluginHandler(manager.Packager(), mdb.JSPluginRepository(), manager, nil, configs, mdb)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	return handler, router
}

func TestCredentialPolicyHandlerValidation(t *testing.T) {
	_, router := newCredentialHandlerHarness(t)

	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/jsplugins/credential-policy", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := put(`{"mode":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want 400", rec.Code)
	}
	if rec := put(`{"mode":"auto","legacy_until":"not-a-time"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid legacy_until status = %d, want 400", rec.Code)
	}
	if rec := put(`{"mode":"strict"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid policy status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jsplugins/credential-policy", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get policy status = %d", rec.Code)
	}
	var policy jsplugin.CredentialPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if policy.EffectiveMode != "strict" {
		t.Fatalf("effective mode = %q, want strict", policy.EffectiveMode)
	}
}

func TestRevokeCredentialsHandlerNotFound(t *testing.T) {
	_, router := newCredentialHandlerHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jsplugins/999/revoke-credentials", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}
