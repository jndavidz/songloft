package jsplugin

import (
	"context"
	"testing"
	"time"

	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"songloft/internal/models"
	"songloft/internal/plugincred"
	"songloft/internal/services"
)

const credentialTestJWTSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type credentialEnv struct {
	manager *Manager
	auth    *services.AuthService
	configs *services.ConfigService
	repo    *database.JSPluginRepository
}

func newCredentialEnv(t *testing.T) *credentialEnv {
	return newCredentialEnvWithSeed(t, nil)
}

func newCredentialEnvWithSeed(t *testing.T, seed func(repo *database.JSPluginRepository)) *credentialEnv {
	t.Helper()
	mdb := testutil.OpenMemoryDB(t)
	if err := mdb.ConfigRepository().Set(context.Background(), &models.Config{
		Key: "jwt_secret", Value: credentialTestJWTSecret,
	}); err != nil {
		t.Fatalf("seed jwt_secret: %v", err)
	}
	auth, err := services.NewAuthService(mdb.ConfigRepository(), mdb.TokenRepository(), "admin", "password")
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(auth.Close)

	if seed != nil {
		seed(mdb.JSPluginRepository())
	}

	pluginsDir := t.TempDir()
	dataDir := t.TempDir()
	manager := NewManager(mdb.JSPluginRepository(), pluginsDir, dataDir, "", nil, mdb)
	configs := services.NewConfigService(mdb.ConfigRepository())
	manager.SetAuthService(auth, "58091")
	manager.SetConfigService(configs)
	t.Cleanup(func() { manager.Close() })

	return &credentialEnv{manager: manager, auth: auth, configs: configs, repo: mdb.JSPluginRepository()}
}

func TestCredentialPolicyDefaultsAndScopes(t *testing.T) {
	t.Run("fresh install starts strict", func(t *testing.T) {
		env := newCredentialEnv(t)
		env.manager.ensureCredentialPolicyDefaults()

		policy := env.manager.GetCredentialPolicy()
		if policy.Mode != credentialModeAuto {
			t.Fatalf("mode = %q, want auto", policy.Mode)
		}
		if policy.EffectiveMode != credentialModeStrict {
			t.Fatalf("fresh install effective mode = %q, want strict", policy.EffectiveMode)
		}
		scopes := env.manager.scopesForPlugin([]string{plugincred.ScopeSongsRead})
		if len(scopes) != 1 || scopes[0] != plugincred.ScopeSongsRead {
			t.Fatalf("fresh install scopes = %v, want [songs.read]", scopes)
		}
	})

	t.Run("upgrade keeps a bounded legacy window", func(t *testing.T) {
		// 模拟升级前已安装插件：库内已有记录，窗口才应打开。
		env := newCredentialEnvWithSeed(t, func(repo *database.JSPluginRepository) {
			if err := repo.Create(context.Background(), &JSPlugin{
				Name: "legacy", Version: "1.0.0", EntryPath: "legacy",
				Main: "main.js", Status: JSPluginStatusActive, FilePath: "legacy.jsplugin.zip",
			}); err != nil {
				t.Fatalf("seed plugin: %v", err)
			}
		})

		policy := env.manager.GetCredentialPolicy()
		if policy.LegacyUntil == "" {
			t.Fatal("legacy_until must be persisted on upgrade")
		}
		if policy.EffectiveMode != credentialModeLegacy {
			t.Fatalf("upgrade effective mode = %q, want legacy", policy.EffectiveMode)
		}
		legacyScopes := env.manager.scopesForPlugin([]string{plugincred.ScopeSongsRead})
		if len(legacyScopes) != len(plugincred.HTTPPluginScopes) {
			t.Fatalf("legacy scopes = %v, want all HTTP scopes", legacyScopes)
		}
	})

	env := newCredentialEnv(t)
	if err := env.configs.Set(PluginCredentialModeConfigKey, credentialModeStrict); err != nil {
		t.Fatalf("set strict: %v", err)
	}
	strictScopes := env.manager.scopesForPlugin([]string{plugincred.ScopeSongsRead})
	if len(strictScopes) != 1 || strictScopes[0] != plugincred.ScopeSongsRead {
		t.Fatalf("strict scopes = %v, want [songs.read]", strictScopes)
	}

	// 通配符糖在签发侧展开为具体 scope。
	wildcard := env.manager.scopesForPlugin([]string{"songs.*"})
	if !plugincred.HasScope(wildcard, plugincred.ScopeSongsRead) || !plugincred.HasScope(wildcard, plugincred.ScopeSongsWrite) {
		t.Fatalf("wildcard scopes = %v", wildcard)
	}
}

func TestCredentialIssueCacheAndRevocation(t *testing.T) {
	env := newCredentialEnv(t)
	ctx := context.Background()
	if err := env.configs.Set(PluginCredentialModeConfigKey, credentialModeStrict); err != nil {
		t.Fatalf("set strict: %v", err)
	}

	first, err := env.manager.issuePluginToken(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	second, err := env.manager.issuePluginToken(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead})
	if err != nil {
		t.Fatalf("issue again: %v", err)
	}
	if first != second {
		t.Fatal("host must cache the credential until refresh lead")
	}

	claims, err := env.auth.ValidateToken(ctx, first)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !plugincred.HasScope(claims.Scopes, plugincred.ScopeSongsRead) {
		t.Fatalf("scopes = %v", claims.Scopes)
	}
	if plugincred.HasScope(claims.Scopes, plugincred.ScopeSongsWrite) {
		t.Fatalf("strict token must not carry write scope: %v", claims.Scopes)
	}

	env.manager.revokePluginCredentials("demo-plugin")
	if _, err := env.auth.ValidateToken(ctx, first); err == nil {
		t.Fatal("revoked credential must be rejected")
	}

	third, err := env.manager.issuePluginToken(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead})
	if err != nil {
		t.Fatalf("issue after revoke: %v", err)
	}
	if third == first {
		t.Fatal("revocation must force a new credential")
	}
	if _, err := env.auth.ValidateToken(ctx, third); err != nil {
		t.Fatalf("new credential: %v", err)
	}

	// 缓存过期（已越过 refresh lead）不能复用：下一次必须重签。
	env.manager.credentialMu.Lock()
	env.manager.credentialCache["demo-plugin"] = cachedCredential{token: "stale", expiresAt: time.Now().Add(-time.Minute)}
	env.manager.credentialMu.Unlock()
	refreshed, err := env.manager.issuePluginToken(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed == "stale" {
		t.Fatal("expired cache entry must be refreshed")
	}
}

func TestCredentialUpdateAndDisableRevoke(t *testing.T) {
	env := newCredentialEnv(t)
	ctx := context.Background()
	env.manager.ensureCredentialPolicyDefaults()

	manifest := testManifest("credential-plugin")
	zipData := createTestPluginZip(t, manifest, simpleJSCode)
	plugin, _, err := env.manager.Packager().InstallFromUpload(zipData)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	token, err := env.manager.issuePluginToken(ctx, plugin.EntryPath, plugin.Permissions)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	updated := testManifest("credential-plugin")
	updated.Version = "1.1.0"
	updated.Permissions = []string{"storage"}
	updatedZip := createTestPluginZip(t, updated, simpleJSCode+";//v1.1.0")
	if _, err := env.manager.Packager().Update(plugin.ID, updatedZip); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := env.auth.ValidateToken(ctx, token); err == nil {
		t.Fatal("update must revoke previously issued credential")
	}

	refreshed, err := env.manager.issuePluginToken(ctx, plugin.EntryPath, updated.Permissions)
	if err != nil {
		t.Fatalf("issue after update: %v", err)
	}
	if err := env.manager.DisablePlugin(ctx, plugin.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := env.auth.ValidateToken(ctx, refreshed); err == nil {
		t.Fatal("disable must revoke previously issued credential")
	}
}

func TestHTTPPluginScopesMatchPermissionConstants(t *testing.T) {
	// plugincred 是独立包（避免 services ↔ jsplugin 循环），scope 字面量必须与
	// manifest 权限常量保持一致；新增 HTTP 可见权限时这里会先失败。
	pairs := map[string]string{
		plugincred.ScopeSongsRead:      PermSongsRead,
		plugincred.ScopeSongsWrite:     PermSongsWrite,
		plugincred.ScopePlaylistsRead:  PermPlaylistsRead,
		plugincred.ScopePlaylistsWrite: PermPlaylistsWrite,
		plugincred.ScopeTagsRead:       PermTagsRead,
		plugincred.ScopeTagsWrite:      PermTagsWrite,
		plugincred.ScopeNet:            PermNet,
	}
	for scope, perm := range pairs {
		if scope != perm {
			t.Fatalf("scope %q != permission constant %q", scope, perm)
		}
	}
	for _, scope := range plugincred.HTTPPluginScopes {
		if _, ok := pairs[scope]; !ok {
			t.Fatalf("HTTP scope %q has no matching permission constant; update plugincred + docs", scope)
		}
	}
}

func TestUpdateCredentialPolicyRevokesAll(t *testing.T) {
	env := newCredentialEnv(t)
	ctx := context.Background()
	env.manager.ensureCredentialPolicyDefaults()

	token, err := env.manager.issuePluginToken(ctx, "demo-plugin", []string{plugincred.ScopeSongsRead})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if err := env.manager.UpdateCredentialPolicy(ctx, credentialModeStrict, ""); err != nil {
		t.Fatalf("update policy: %v", err)
	}
	if _, err := env.auth.ValidateToken(ctx, token); err == nil {
		t.Fatal("switching scope profile must revoke existing credentials")
	}
	if got := env.manager.GetCredentialPolicy().EffectiveMode; got != credentialModeStrict {
		t.Fatalf("effective mode = %q, want strict", got)
	}

	if err := env.manager.UpdateCredentialPolicy(ctx, "bogus", ""); err == nil {
		t.Fatal("invalid mode must be rejected")
	}
	if err := env.manager.UpdateCredentialPolicy(ctx, credentialModeAuto, "not-a-time"); err == nil {
		t.Fatal("invalid legacy_until must be rejected")
	}

	// 管理员全量撤销同时关闭旧票据窗口。
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := env.configs.Set(PluginCredentialLegacyUntilConfigKey, future); err != nil {
		t.Fatalf("seed window: %v", err)
	}
	if err := env.manager.RevokeAllCredentials(ctx); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	policy := env.manager.GetCredentialPolicy()
	if policy.EffectiveMode != credentialModeStrict {
		t.Fatalf("legacy window must be closed, effective mode = %q", policy.EffectiveMode)
	}
	if policy.LegacyUntil == "" {
		t.Fatal("legacy_until should be persisted even when closed")
	}
}
