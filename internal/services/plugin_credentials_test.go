package services

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"songloft/internal/models"
	"songloft/internal/plugincred"
)

func newTestAuthService(t *testing.T, env *authTestEnv) *AuthService {
	t.Helper()
	env.seedJWTSecret(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	svc, err := NewAuthService(env.configs, env.tokens, "admin", "password")
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func TestPluginCredentialIssueAndValidate(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)
	ctx := context.Background()

	cred, err := svc.IssuePluginCredential(ctx, "demo-plugin", []string{
		plugincred.ScopeSongsRead,
		plugincred.ScopeSongsRead,
		plugincred.ScopePlaylistsRead,
	}, time.Minute)
	if err != nil {
		t.Fatalf("IssuePluginCredential: %v", err)
	}

	claims, err := svc.ValidateToken(ctx, cred.Token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Kind != KindPlugin {
		t.Fatalf("Kind = %q, want %q", claims.Kind, KindPlugin)
	}
	if claims.PluginEntry != "demo-plugin" {
		t.Fatalf("PluginEntry = %q", claims.PluginEntry)
	}
	if claims.ClientID != "plugin:demo-plugin" {
		t.Fatalf("ClientID = %q", claims.ClientID)
	}
	if len(claims.Scopes) != 2 {
		t.Fatalf("Scopes = %v, want deduped 2 entries", claims.Scopes)
	}
	if !plugincred.HasScope(claims.Scopes, plugincred.ScopeSongsRead) ||
		!plugincred.HasScope(claims.Scopes, plugincred.ScopePlaylistsRead) {
		t.Fatalf("Scopes = %v", claims.Scopes)
	}
}

func TestPluginCredentialRevocationIsPerPlugin(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)
	ctx := context.Background()

	a, err := svc.IssuePluginCredential(ctx, "plugin-a", []string{plugincred.ScopeSongsRead}, time.Minute)
	if err != nil {
		t.Fatalf("issue a: %v", err)
	}
	b, err := svc.IssuePluginCredential(ctx, "plugin-b", []string{plugincred.ScopeSongsRead}, time.Minute)
	if err != nil {
		t.Fatalf("issue b: %v", err)
	}

	if err := svc.RevokePluginCredentials(ctx, "plugin-a"); err != nil {
		t.Fatalf("revoke a: %v", err)
	}

	if _, err := svc.ValidateToken(ctx, a.Token); err == nil {
		t.Fatal("revoked plugin credential must be rejected")
	}
	if _, err := svc.ValidateToken(ctx, b.Token); err != nil {
		t.Fatalf("other plugin credential must stay valid: %v", err)
	}

	// 重新签发后新票据可用，旧票据仍然无效。
	fresh, err := svc.IssuePluginCredential(ctx, "plugin-a", []string{plugincred.ScopeSongsRead}, time.Minute)
	if err != nil {
		t.Fatalf("reissue a: %v", err)
	}
	if _, err := svc.ValidateToken(ctx, fresh.Token); err != nil {
		t.Fatalf("fresh credential: %v", err)
	}
}

func TestPluginCredentialRestartInvalidatesOldTokens(t *testing.T) {
	env := newAuthTestEnv(t)
	first := newTestAuthService(t, env)
	ctx := context.Background()

	cred, err := first.IssuePluginCredential(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead}, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := first.ValidateToken(ctx, cred.Token); err != nil {
		t.Fatalf("pre-restart validate: %v", err)
	}

	second, err := NewAuthService(env.configs, env.tokens, "admin", "password")
	if err != nil {
		t.Fatalf("second NewAuthService: %v", err)
	}
	t.Cleanup(second.Close)

	if _, err := second.ValidateToken(ctx, cred.Token); err == nil {
		t.Fatal("credential issued before restart must be rejected")
	}
	fresh, err := second.IssuePluginCredential(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead}, time.Hour)
	if err != nil {
		t.Fatalf("issue after restart: %v", err)
	}
	if _, err := second.ValidateToken(ctx, fresh.Token); err != nil {
		t.Fatalf("fresh credential after restart: %v", err)
	}
}

func TestPluginCredentialExpiry(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)

	cred, err := svc.IssuePluginCredential(context.Background(), "demo-plugin", []string{plugincred.ScopeSongsRead}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := svc.ValidateToken(context.Background(), cred.Token); err == nil {
		t.Fatal("expired plugin credential must be rejected")
	}
}

func TestLegacyPluginSystemTokenWindow(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)
	ctx := context.Background()

	claims := &Claims{
		ClientID: "plugin-system",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(100 * 365 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        "legacy",
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(svc.secret)
	if err != nil {
		t.Fatalf("sign legacy: %v", err)
	}

	// 未配置窗口：fail closed。
	if _, err := svc.ValidateToken(ctx, token); err == nil {
		t.Fatal("legacy token must be rejected when no compatibility window exists")
	}

	// 窗口内：接受，但降级为插件 scope 的 legacy 主体。
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := env.configs.Set(ctx, &models.Config{Key: plugincred.LegacyUntilConfigKey, Value: until}); err != nil {
		t.Fatalf("seed window: %v", err)
	}
	accepted, err := svc.ValidateToken(ctx, token)
	if err != nil {
		t.Fatalf("legacy token within window: %v", err)
	}
	if !accepted.Legacy || accepted.Kind != KindPlugin || accepted.ClientID != "plugin:legacy" {
		t.Fatalf("legacy claims = %+v", accepted)
	}
	if !plugincred.HasScope(accepted.Scopes, plugincred.ScopeSongsRead) {
		t.Fatalf("legacy scopes = %v", accepted.Scopes)
	}

	// strict 档显式关闭旧票据，即使窗口未到期。
	if err := env.configs.Set(ctx, &models.Config{Key: plugincred.ModeConfigKey, Value: "strict"}); err != nil {
		t.Fatalf("set strict: %v", err)
	}
	if _, err := svc.ValidateToken(ctx, token); err == nil {
		t.Fatal("strict mode must close the legacy window")
	}
	if err := env.configs.Set(ctx, &models.Config{Key: plugincred.ModeConfigKey, Value: "auto"}); err != nil {
		t.Fatalf("restore auto: %v", err)
	}

	// 窗口结束后：拒绝。
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := env.configs.Set(ctx, &models.Config{Key: plugincred.LegacyUntilConfigKey, Value: past}); err != nil {
		t.Fatalf("update window: %v", err)
	}
	if _, err := svc.ValidateToken(ctx, token); err == nil {
		t.Fatal("legacy token must be rejected after the window closes")
	}
}

func TestSystemTokenValidation(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)

	token, exp, err := svc.IssueSystemToken(time.Minute)
	if err != nil {
		t.Fatalf("IssueSystemToken: %v", err)
	}
	if time.Until(exp) <= 0 {
		t.Fatal("system token already expired")
	}
	claims, err := svc.ValidateToken(context.Background(), token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Kind != KindSystem || claims.ClientID != "system" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestRevokeAllPluginCredentials(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)
	ctx := context.Background()

	cred, err := svc.IssuePluginCredential(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead}, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := svc.RevokeAllPluginCredentials(ctx); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if _, err := svc.ValidateToken(ctx, cred.Token); err == nil {
		t.Fatal("credential must be rejected after global revocation")
	}
}

func TestUserTokenStillValidatedAgainstDB(t *testing.T) {
	env := newAuthTestEnv(t)
	svc := newTestAuthService(t, env)
	ctx := context.Background()

	login, err := svc.Login(ctx, "admin", "password", "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	claims, err := svc.ValidateToken(ctx, login.AccessToken)
	if err != nil {
		t.Fatalf("validate user token: %v", err)
	}
	if claims.Kind != "" || claims.ClientID == "" {
		t.Fatalf("user claims = %+v", claims)
	}
}
