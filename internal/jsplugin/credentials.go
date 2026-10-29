package jsplugin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"songloft/internal/plugincred"
	"songloft/internal/services"
)

// 插件凭据策略（S-02）
//
// 票据有效期由 services.PluginCredentialTTL 决定；宿主按插件主体缓存并在
// 过期前重签，插件通过 bridge 的 plugin.getToken 拿到当前票据。
//
// scope 有两档：
//   - strict：manifest permissions 展开后的最小 scope（目标态）；
//   - legacy：全部插件 HTTP scope，作为旧插件的短并存窗口。
//
// 窗口由 plugin_credential_legacy_until 控制。升级时默认保留 14 天；
// 全新安装没有存量插件，窗口直接关闭（从第一天起就是最小 scope）。
// auto 档在窗口内 legacy、到期自动 strict。

const (
	// PluginCredentialModeConfigKey 凭据模式：auto（默认，窗口内 legacy）/legacy/strict。
	PluginCredentialModeConfigKey = plugincred.ModeConfigKey
	// PluginCredentialLegacyUntilConfigKey 兼容窗口截止时间（RFC3339）。
	PluginCredentialLegacyUntilConfigKey = plugincred.LegacyUntilConfigKey

	credentialModeAuto   = string(plugincred.ModeAuto)
	credentialModeLegacy = string(plugincred.ModeLegacy)
	credentialModeStrict = string(plugincred.ModeStrict)

	// defaultLegacyWindow 升级场景的默认兼容窗口长度。
	defaultLegacyWindow = 14 * 24 * time.Hour
	// credentialRefreshLead 提前重签的时间窗口。
	credentialRefreshLead = 5 * time.Minute
)

type cachedCredential struct {
	token     string
	expiresAt time.Time
}

// CredentialPolicy 是插件凭据策略的可读视图。
type CredentialPolicy struct {
	Mode          string `json:"mode"`
	LegacyUntil   string `json:"legacy_until,omitempty"`
	EffectiveMode string `json:"effective_mode"`
}

// ensureCredentialPolicyDefaults 首次启动时固化兼容窗口，避免「now+14 天」
// 随读随算导致窗口永不结束。全新安装（库内没有插件）直接关闭窗口：
// 没有旧消费方需要兼容，从第一天起就使用 manifest 最小 scope。
func (m *Manager) ensureCredentialPolicyDefaults() {
	if m.configService == nil {
		return
	}
	if m.configService.GetString(PluginCredentialModeConfigKey, "") == "" {
		if err := m.configService.Set(PluginCredentialModeConfigKey, credentialModeAuto); err != nil {
			slog.Warn("初始化插件凭据模式失败", "error", err)
		}
	}
	if m.configService.GetString(PluginCredentialLegacyUntilConfigKey, "") != "" {
		return
	}

	until := time.Now().UTC()
	if plugins, err := m.repo.GetAll(context.Background()); err == nil && len(plugins) > 0 {
		until = time.Now().Add(defaultLegacyWindow).UTC()
	}
	if err := m.configService.Set(PluginCredentialLegacyUntilConfigKey, until.Format(time.RFC3339)); err != nil {
		slog.Warn("初始化插件凭据兼容窗口失败", "error", err)
	}
}

// GetCredentialPolicy 返回当前策略与生效档位。
func (m *Manager) GetCredentialPolicy() CredentialPolicy {
	mode := plugincred.ModeAuto
	untilRaw := ""
	if m.configService != nil {
		mode = plugincred.Mode(m.configService.GetString(PluginCredentialModeConfigKey, string(plugincred.ModeAuto)))
		untilRaw = m.configService.GetString(PluginCredentialLegacyUntilConfigKey, "")
	}
	if !mode.Valid() {
		mode = plugincred.ModeStrict
	}
	effective := plugincred.EffectiveMode(mode, untilRaw, time.Now())
	return CredentialPolicy{Mode: string(mode), LegacyUntil: untilRaw, EffectiveMode: string(effective)}
}

// ValidateCredentialPolicyInput 校验管理员输入的凭据策略。
func ValidateCredentialPolicyInput(mode, legacyUntil string) error {
	if !plugincred.Mode(mode).Valid() {
		return fmt.Errorf("unsupported credential mode %q", mode)
	}
	if legacyUntil != "" {
		if _, err := time.Parse(time.RFC3339, legacyUntil); err != nil {
			return fmt.Errorf("legacy_until must be RFC3339: %w", err)
		}
	}
	return nil
}

// UpdateCredentialPolicy 由管理员设置模式与兼容窗口。
// 切换档位后立即吊销全部插件票据，避免旧 scope 在有效期内继续生效。
func (m *Manager) UpdateCredentialPolicy(ctx context.Context, mode, legacyUntil string) error {
	if err := ValidateCredentialPolicyInput(mode, legacyUntil); err != nil {
		return err
	}
	modeValue := plugincred.Mode(mode)
	if m.configService == nil {
		return fmt.Errorf("config service unavailable")
	}
	if err := m.configService.Set(PluginCredentialModeConfigKey, string(modeValue)); err != nil {
		return err
	}
	if legacyUntil != "" {
		if err := m.configService.Set(PluginCredentialLegacyUntilConfigKey, legacyUntil); err != nil {
			return err
		}
	}
	return m.revokeAllScopedCredentials(ctx)
}

// ApplyCredentialPolicyConfigChange 处理通用 /configs 入口对凭据策略的修改。
// 通用 KV 不经过 UpdateCredentialPolicy，必须在这里补齐吊销副作用。
func (m *Manager) ApplyCredentialPolicyConfigChange() {
	if err := m.revokeAllScopedCredentials(context.Background()); err != nil {
		slog.Warn("配置变更后吊销插件凭据失败", "error", err)
	}
}

// RevokeAllCredentials 撤销全部插件票据并立即关闭旧票据兼容窗口。
// 管理端在需要立刻停用旧消费方时使用。
func (m *Manager) RevokeAllCredentials(ctx context.Context) error {
	if m.configService != nil {
		now := time.Now().UTC().Format(time.RFC3339)
		if err := m.configService.Set(PluginCredentialLegacyUntilConfigKey, now); err != nil {
			return err
		}
	}
	return m.revokeAllScopedCredentials(ctx)
}

func (m *Manager) revokeAllScopedCredentials(ctx context.Context) error {
	if m.authService != nil {
		if err := m.authService.RevokeAllPluginCredentials(ctx); err != nil {
			return err
		}
	}
	m.clearCredentialCache()
	return nil
}

// scopesForPlugin 计算插件的 HTTP scope 集合。
func (m *Manager) scopesForPlugin(permissions []string) []string {
	if m.GetCredentialPolicy().EffectiveMode == credentialModeLegacy {
		return append([]string(nil), plugincred.HTTPPluginScopes...)
	}
	scopes := make([]string, 0, len(plugincred.HTTPPluginScopes))
	for _, scope := range plugincred.HTTPPluginScopes {
		if CheckPermission(permissions, scope) {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

// issuePluginToken 签发（或复用）插件短期票据。
func (m *Manager) issuePluginToken(ctx context.Context, entryPath string, permissions []string) (string, error) {
	if m.authService == nil {
		return "", fmt.Errorf("auth service unavailable")
	}
	if entryPath == "" {
		return "", fmt.Errorf("plugin entry path missing")
	}

	m.credentialMu.Lock()
	defer m.credentialMu.Unlock()

	if cached, ok := m.credentialCache[entryPath]; ok && time.Until(cached.expiresAt) > credentialRefreshLead {
		return cached.token, nil
	}

	credential, err := m.authService.IssuePluginCredential(ctx, entryPath, m.scopesForPlugin(permissions), services.PluginCredentialTTL)
	if err != nil {
		return "", err
	}
	m.credentialCache[entryPath] = cachedCredential{token: credential.Token, expiresAt: credential.ExpiresAt}
	return credential.Token, nil
}

// tokenProviderFor 返回绑定到具体插件的票据提供方（bridge 使用）。
func (m *Manager) tokenProviderFor(plugin *JSPlugin) func(context.Context) (string, error) {
	entryPath := plugin.EntryPath
	permissions := append([]string(nil), plugin.Permissions...)
	return func(ctx context.Context) (string, error) {
		return m.issuePluginToken(ctx, entryPath, permissions)
	}
}

// tokenForEntry 为内部转发获取插件票据；插件必须已加载或可从库中读取权限。
func (m *Manager) tokenForEntry(ctx context.Context, entryPath string) (string, error) {
	if svc, ok := m.GetService(entryPath); ok {
		return m.issuePluginToken(ctx, entryPath, svc.plugin.Permissions)
	}
	plugin, err := m.repo.GetByEntryPath(ctx, entryPath)
	if err != nil {
		return "", fmt.Errorf("plugin %q not found: %w", entryPath, err)
	}
	if plugin == nil {
		return "", fmt.Errorf("plugin %q not found", entryPath)
	}
	return m.issuePluginToken(ctx, entryPath, plugin.Permissions)
}

// revokePluginCredentials 撤销某插件的票据并清空宿主缓存。
// 先递增代次再清缓存：反序会让并发的签发把旧代次票据重新缓存起来。
func (m *Manager) revokePluginCredentials(entryPath string) {
	if m.authService != nil && entryPath != "" {
		if err := m.authService.RevokePluginCredentials(context.Background(), entryPath); err != nil {
			slog.Warn("撤销插件凭据失败", "plugin", entryPath, "error", err)
		}
	}

	m.credentialMu.Lock()
	delete(m.credentialCache, entryPath)
	m.credentialMu.Unlock()
}

// RevokePluginCredentials 供管理端点显式撤销某插件票据。
func (m *Manager) RevokePluginCredentials(ctx context.Context, pluginID int64) error {
	plugin, err := m.repo.GetByID(ctx, pluginID)
	if err != nil {
		return err
	}
	m.revokePluginCredentials(plugin.EntryPath)
	return nil
}

func (m *Manager) clearCredentialCache() {
	m.credentialMu.Lock()
	clear(m.credentialCache)
	m.credentialMu.Unlock()
}
