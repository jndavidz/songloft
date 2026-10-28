package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"songloft/internal/middleware"
	"songloft/internal/models"
)

func TestPersonalTokenHTTP(t *testing.T) {
	h := newAuthHandlerForTest(t)
	login, err := h.authService.Login(context.Background(), "admin", "password", "browser")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := h.authService.GeneratePluginToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(middleware.AuthMiddleware(h.authService))
	r.Get("/resource", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireLoginSession)
		r.Post("/auth/personal-tokens", h.CreatePersonalToken)
		r.Get("/auth/personal-tokens", h.ListPersonalTokens)
		r.Delete("/auth/personal-tokens/{id}", h.RevokePersonalToken)
		r.Get("/auth/tokens", h.ListTokens)
	})
	request := func(method, path, token, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("%s %s: status %d want %d", method, path, rr.Code, want)
		}
		return rr
	}
	const path = "/auth/personal-tokens"
	request("POST", path, "", `{"name":"HA"}`, 401)
	request("POST", path, login.AccessToken, `{"name":" "}`, 400)
	request("POST", path, login.AccessToken, `{`, 400)
	created := request("POST", path, login.AccessToken, `{"name":"HA"}`, 201)
	if created.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret response is cacheable")
	}
	var token models.CreatePersonalTokenResponse
	if err := json.Unmarshal(created.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{token.Token, login.RefreshToken, plugin} {
		request("POST", path, raw, `{"name":"bypass"}`, 403)
		request("GET", path, raw, "", 403)
		request("DELETE", path+"/"+token.ID, raw, "", 403)
		request("GET", "/auth/tokens", raw, "", 403)
	}
	request("GET", "/resource", token.Token, "", 200)
	request("GET", "/resource?access_token="+token.Token, "", "", 200)
	list := request("GET", path+"?limit=1&offset=0", login.AccessToken, "", 200)
	if strings.Contains(list.Body.String(), token.Token) || strings.Contains(list.Body.String(), "token_hash") {
		t.Fatal("secret exposed in list")
	}
	request("DELETE", path+"/missing", login.AccessToken, "", 404)
	request("DELETE", path+"/"+token.ID, login.AccessToken, "", 204)
	request("DELETE", path+"/"+token.ID, login.AccessToken, "", 204)
	request("GET", "/resource", token.Token, "", 401)
	request("GET", "/resource?access_token="+token.Token, "", "", 401)
}
