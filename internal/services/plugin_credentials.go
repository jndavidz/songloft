package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"songloft/internal/database"
	"songloft/internal/models"
	"songloft/internal/plugincred"
)

// 插件凭据（S-02）：
//
// 此前插件拿到的是 100 年有效、全插件共用、验证时跳过数据库撤销的
// "plugin-system" JWT（GeneratePluginToken）。重启不会废除旧签名，撤销也没有
// 落点。现在改为按插件主体签发短期票据：
//
//   - scope 来自 manifest 权限（strict）或兼容档的全插件 scope（legacy）；
//   - 每次进程启动 global 代次 +1，所有旧签名立即失效；
//   - 按插件撤销/更新/禁用/卸载时该插件代次 +1；
//   - 代次持久化在 configs 表，缓存不吃掉撤销（插件票据不走 tokenCache）。
//
// 旧的 plugin-system 票据（Kind 为空）一律拒绝，不再作为撤销旁路保留。

const (
	// PluginCredentialTTL 插件票据有效期。短期但仍可被宿主缓存在一次
	// 调用链内复用；过期由宿主重新签发，插件无感。
	PluginCredentialTTL = time.Hour
	// SystemCredentialTTL 宿主内部调用票据有效期（仅进程内使用）。
	SystemCredentialTTL = 24 * time.Hour

	// pluginCredentialStateKey 代次状态在 configs 表中的键。
	pluginCredentialStateKey = "plugin_credential_state"

	// KindPlugin / KindSystem 标记票据类别；Kind 为空是历史用户票据。
	KindPlugin = "plugin"
	KindSystem = "system"
)

// pluginCredentialState 是持久化的插件凭据代次。
// Global 每次进程启动递增；Plugins 按插件 entryPath 记录撤销代次。
type pluginCredentialState struct {
	Global  int64            `json:"global"`
	Plugins map[string]int64 `json:"plugins,omitempty"`
}

// PluginCredentialInfo 是签发给调用方观察的票据元数据。
type PluginCredentialInfo struct {
	Token     string
	ExpiresAt time.Time
}

// ensurePluginCredentialState 首次使用时加载持久化状态并递增 global 代次。
// 懒加载而非构造函数中执行：避免只读场景也写库，也让「重启」语义由
// AuthService 实例边界自然表达（测试可直接构造第二个实例模拟重启）。
func (s *AuthService) ensurePluginCredentialState(ctx context.Context) error {
	s.pluginStateMu.Lock()
	defer s.pluginStateMu.Unlock()
	if s.pluginStateReady {
		return nil
	}

	var state pluginCredentialState
	config, err := s.configs.Get(ctx, pluginCredentialStateKey)
	switch {
	case err == nil:
		if config.Value != "" {
			if err := json.Unmarshal([]byte(config.Value), &state); err != nil {
				return fmt.Errorf("解析插件凭据代次失败: %w", err)
			}
		}
	case errors.Is(err, database.ErrNotFound):
		// 首次运行，从零开始。
	default:
		return fmt.Errorf("读取插件凭据代次失败: %w", err)
	}

	state.Global++
	if state.Plugins == nil {
		state.Plugins = map[string]int64{}
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("编码插件凭据代次失败: %w", err)
	}
	if err := s.configs.Set(ctx, &models.Config{Key: pluginCredentialStateKey, Value: string(encoded)}); err != nil {
		return fmt.Errorf("持久化插件凭据代次失败: %w", err)
	}

	s.pluginState = state
	s.pluginStateReady = true
	return nil
}

// persistPluginCredentialStateLocked 在当前锁内持久化状态。
func (s *AuthService) persistPluginCredentialStateLocked(ctx context.Context) error {
	encoded, err := json.Marshal(s.pluginState)
	if err != nil {
		return fmt.Errorf("编码插件凭据代次失败: %w", err)
	}
	if err := s.configs.Set(ctx, &models.Config{Key: pluginCredentialStateKey, Value: string(encoded)}); err != nil {
		return fmt.Errorf("持久化插件凭据代次失败: %w", err)
	}
	return nil
}

// IssuePluginCredential 为指定插件签发短期票据。
//
// entryPath 是插件主体；scopes 必须是已展开的具体 HTTP scope（见
// internal/plugincred）。票据带 global 与插件两条代次，任一不匹配即拒绝。
func (s *AuthService) IssuePluginCredential(ctx context.Context, entryPath string, scopes []string, ttl time.Duration) (*PluginCredentialInfo, error) {
	if entryPath == "" {
		return nil, errors.New("插件凭据缺少插件主体")
	}
	if ttl <= 0 {
		ttl = PluginCredentialTTL
	}
	if err := s.ensurePluginCredentialState(ctx); err != nil {
		return nil, err
	}

	s.pluginStateMu.RLock()
	globalGen := s.pluginState.Global
	pluginGen := s.pluginState.Plugins[entryPath]
	s.pluginStateMu.RUnlock()

	now := time.Now()
	tokenID, err := generateRandomString(32)
	if err != nil {
		return nil, fmt.Errorf("生成插件凭据 ID 失败: %w", err)
	}

	claims := &Claims{
		ClientID:    "plugin:" + entryPath,
		Kind:        KindPlugin,
		PluginEntry: entryPath,
		Scopes:      dedupeStrings(scopes),
		Gen:         globalGen,
		PluginGen:   pluginGen,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        tokenID,
		},
	}
	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return nil, fmt.Errorf("签发插件凭据失败: %w", err)
	}
	return &PluginCredentialInfo{Token: tokenString, ExpiresAt: now.Add(ttl)}, nil
}

// IssueSystemToken 签发宿主内部调用票据。
// 与插件票据分开：内部调用不伪装成插件主体，也不进入任何插件 scope 判定。
func (s *AuthService) IssueSystemToken(ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = SystemCredentialTTL
	}
	now := time.Now()
	tokenID, err := generateRandomString(32)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("生成系统凭据 ID 失败: %w", err)
	}
	claims := &Claims{
		ClientID: "system",
		Kind:     KindSystem,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        tokenID,
		},
	}
	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("签发系统凭据失败: %w", err)
	}
	return tokenString, now.Add(ttl), nil
}

// RevokePluginCredentials 撤销某插件的全部票据（代次 +1，持久化）。
// 下一次签发使用新代次；已签发票据在下一次校验时被拒绝。
func (s *AuthService) RevokePluginCredentials(ctx context.Context, entryPath string) error {
	if entryPath == "" {
		return errors.New("撤销插件凭据缺少插件主体")
	}
	if err := s.ensurePluginCredentialState(ctx); err != nil {
		return err
	}

	s.pluginStateMu.Lock()
	defer s.pluginStateMu.Unlock()
	s.pluginState.Plugins[entryPath]++
	return s.persistPluginCredentialStateLocked(ctx)
}

// RevokeAllPluginCredentials 撤销全部插件票据（global 代次 +1）。
func (s *AuthService) RevokeAllPluginCredentials(ctx context.Context) error {
	if err := s.ensurePluginCredentialState(ctx); err != nil {
		return err
	}

	s.pluginStateMu.Lock()
	defer s.pluginStateMu.Unlock()
	s.pluginState.Global++
	return s.persistPluginCredentialStateLocked(ctx)
}

// legacyPluginWindowOpen 判断旧插件票据是否仍在兼容窗口内。
// 模式与截止时间持久化在 configs 表；缺失或无法解析按已结束处理（fail closed）。
func (s *AuthService) legacyPluginWindowOpen(ctx context.Context) error {
	if s.configs == nil {
		return errors.New("旧插件凭据兼容窗口不可用")
	}
	mode := plugincred.ModeAuto
	if config, err := s.configs.Get(ctx, plugincred.ModeConfigKey); err == nil {
		mode = plugincred.Mode(config.Value)
	}
	untilRaw := ""
	if config, err := s.configs.Get(ctx, plugincred.LegacyUntilConfigKey); err == nil {
		untilRaw = config.Value
	}
	if !plugincred.LegacyWindowOpen(mode, untilRaw, time.Now()) {
		return errors.New("旧插件凭据兼容窗口已结束")
	}
	return nil
}

// validatePluginCredential 校验插件票据的代次与主体。
func (s *AuthService) validatePluginCredential(ctx context.Context, claims *Claims) error {
	if claims.PluginEntry == "" {
		return errors.New("插件凭据缺少插件主体")
	}
	if err := s.ensurePluginCredentialState(ctx); err != nil {
		return err
	}
	s.pluginStateMu.RLock()
	globalGen := s.pluginState.Global
	pluginGen := s.pluginState.Plugins[claims.PluginEntry]
	s.pluginStateMu.RUnlock()

	if claims.Gen != globalGen {
		return errors.New("插件凭据已失效（宿主重启或全局撤销）")
	}
	if claims.PluginGen != pluginGen {
		return errors.New("插件凭据已被撤销")
	}
	return nil
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	return result
}
