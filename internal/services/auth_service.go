package services

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"songloft/internal/database"
	"songloft/internal/models"
	"songloft/internal/plugincred"
)

// TokenRepository 认证令牌仓储接口（AuthService 依赖）。
type TokenRepository interface {
	Create(ctx context.Context, token *models.AuthToken) error
	GetByID(ctx context.Context, tokenID string) (*models.AuthToken, error)
	Revoke(ctx context.Context, tokenID, revokedBy, reason string) error
	ListActive(ctx context.Context, filter *database.TokenFilter) ([]*models.AuthToken, error)
	CleanExpired(ctx context.Context) (int64, error)
	IsRevoked(ctx context.Context, tokenID string) (bool, error)
	CreatePersonalToken(ctx context.Context, token models.PersonalToken, hash string) error
	GetPersonalTokenByHash(ctx context.Context, hash string) (*models.PersonalToken, error)
	ListPersonalTokens(ctx context.Context, limit, offset int) ([]models.PersonalToken, int64, error)
	RevokePersonalToken(ctx context.Context, id string) error
	TouchPersonalToken(ctx context.Context, id string, now time.Time) error
}

// TokenCacheEntry Token 缓存条目
type TokenCacheEntry struct {
	Claims    *Claims
	ExpiresAt time.Time
	Revoked   bool
}

// AuthService 认证服务
type AuthService struct {
	tokens       TokenRepository
	configs      ConfigRepository
	secret       []byte
	username     string
	passwordHash []byte
	// Token 内存缓存，key 为 token 字符串，value 为缓存条目。
	// 仅用于用户票据；插件/系统票据按请求校验，撤销立刻生效。
	tokenCache sync.Map // map[string]*TokenCacheEntry
	done       chan struct{}
	closeOnce  sync.Once

	// 插件凭据代次（S-02），见 plugin_credentials.go。
	pluginStateMu    sync.RWMutex
	pluginState      pluginCredentialState
	pluginStateReady bool
}

// Claims JWT声明结构
type Claims struct {
	ClientID string `json:"client_id"`
	// Kind 区分票据类别：空=历史用户票据，plugin=插件票据，system=宿主内部调用。
	Kind string `json:"kind,omitempty"`
	// PluginEntry 是插件票据的插件主体（entry_path）。
	PluginEntry string `json:"plugin_entry,omitempty"`
	// Scopes 是插件票据的 HTTP 授权范围（manifest 权限展开后的具体值）。
	Scopes []string `json:"scopes,omitempty"`
	// Gen / PluginGen 是签发时的全局与插件代次，任一不匹配即撤销/重启失效。
	Gen       int64 `json:"gen,omitempty"`
	PluginGen int64 `json:"plugin_gen,omitempty"`
	// Legacy 标记为窗口内被接受的旧 plugin-system 票据（非签发主体）。
	Legacy bool `json:"-"`
	// CredentialType 从持久记录确定，不信任请求声明。
	CredentialType string `json:"-"`
	jwt.RegisteredClaims
}

// RefreshResponse 刷新Token响应
type RefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// NewAuthService 创建认证服务
func NewAuthService(configs ConfigRepository, tokens TokenRepository, username, password string) (*AuthService, error) {
	// 从数据库获取 JWT 密钥
	config, err := configs.Get(context.Background(), "jwt_secret")
	if err != nil {
		return nil, fmt.Errorf("failed to get jwt secret: %w", err)
	}

	// 解码密钥
	secret, err := hex.DecodeString(config.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to decode jwt secret: %w", err)
	}

	// 启动时将管理员密码哈希后常驻内存，避免明文长期留存；登录时用 bcrypt 常量时间比较。
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash admin password: %w", err)
	}

	s := &AuthService{
		tokens:       tokens,
		configs:      configs,
		secret:       secret,
		username:     username,
		passwordHash: passwordHash,
		done:         make(chan struct{}),
	}

	// 启动缓存清理协程
	go s.startCacheCleanup()

	return s, nil
}

// GenerateSecret 生成新的 JWT 密钥
func GenerateSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// getAdminUsername 获取管理员用户名
func (s *AuthService) getAdminUsername(ctx context.Context) (string, error) {
	return s.username, nil
}

// Login 用户登录
func (s *AuthService) Login(ctx context.Context, username, password, clientInfo string) (*models.LoginResponse, error) {
	// 从数据库获取管理员账户信息
	adminUsername, err := s.getAdminUsername(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get admin username: %w", err)
	}

	// 两项都算完再判断，避免因提前返回形成用户名/密码的时序侧信道。
	usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(adminUsername)) == 1
	passwordMatch := bcrypt.CompareHashAndPassword(s.passwordHash, []byte(password)) == nil
	if !usernameMatch || !passwordMatch {
		return nil, fmt.Errorf("invalid credentials")
	}

	// 生成客户端 ID
	clientID, err := generateClientID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate client id: %w", err)
	}

	// 生成 Access Token (7 天过期)
	accessToken, accessExp, err := s.generateToken(clientID, "access", 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("failed to generate access token: %w", err)
	}

	// 生成 Refresh Token (30 天过期)
	refreshToken, refreshExp, err := s.generateToken(clientID, "refresh", 30*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("failed to generate refresh token: %w", err)
	}

	// 保存 Token 到数据库
	now := time.Now()
	accessRecord := &models.AuthToken{
		TokenID:    accessToken,
		TokenType:  "access",
		ClientInfo: clientInfo,
		ExpiresAt:  accessExp,
		CreatedAt:  now,
	}

	refreshRecord := &models.AuthToken{
		TokenID:    refreshToken,
		TokenType:  "refresh",
		ClientInfo: clientInfo,
		ExpiresAt:  refreshExp,
		CreatedAt:  now,
	}

	if err := s.tokens.Create(ctx, accessRecord); err != nil {
		return nil, fmt.Errorf("failed to save access token: %w", err)
	}

	if err := s.tokens.Create(ctx, refreshRecord); err != nil {
		return nil, fmt.Errorf("failed to save refresh token: %w", err)
	}

	// 清理过期 Token
	_, _ = s.tokens.CleanExpired(ctx)

	return &models.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int64(accessExp.Sub(now).Seconds()),
		TokenType:    "Bearer",
	}, nil
}

// getCachedToken 从缓存获取 Token 信息
func (s *AuthService) getCachedToken(tokenString string) (*TokenCacheEntry, bool) {
	if entry, ok := s.tokenCache.Load(tokenString); ok {
		cacheEntry := entry.(*TokenCacheEntry)
		// 检查缓存是否过期
		if time.Now().Before(cacheEntry.ExpiresAt) && !cacheEntry.Revoked {
			return cacheEntry, true
		}
		// 缓存已过期，删除它
		s.tokenCache.Delete(tokenString)
	}
	return nil, false
}

// setTokenCache 设置 Token 缓存
func (s *AuthService) setTokenCache(tokenString string, claims *Claims, expiresAt time.Time, revoked bool) {
	s.tokenCache.Store(tokenString, &TokenCacheEntry{
		Claims:    claims,
		ExpiresAt: expiresAt,
		Revoked:   revoked,
	})
}

// deleteTokenCache 删除 Token 缓存
func (s *AuthService) deleteTokenCache(tokenString string) {
	s.tokenCache.Delete(tokenString)
}

// Close 关闭 AuthService，停止缓存清理协程
func (s *AuthService) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

// startCacheCleanup 启动缓存清理协程，每分钟清理一次过期缓存
func (s *AuthService) startCacheCleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			s.tokenCache.Range(func(key, value interface{}) bool {
				entry := value.(*TokenCacheEntry)
				// 如果缓存已过期或 token 已被撤销，删除它
				if now.After(entry.ExpiresAt) || entry.Revoked {
					s.tokenCache.Delete(key)
				}
				return true
			})
		case <-s.done:
			return
		}
	}
}

// Logout 用户登出
func (s *AuthService) Logout(ctx context.Context, accessToken, clientID string) error {
	// 撤销 Access Token
	if err := s.tokens.Revoke(ctx, accessToken, clientID, "logout"); err != nil {
		return fmt.Errorf("failed to revoke access token: %w", err)
	}
	// 清除缓存
	s.deleteTokenCache(accessToken)

	// 查找并撤销对应的 Refresh Token
	filter := &database.TokenFilter{
		TokenType: "refresh",
	}

	tokens, err := s.tokens.ListActive(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to list refresh tokens: %w", err)
	}

	for _, token := range tokens {
		// 解析 refresh token 的 JWT 获取 ClientID，按 clientID 匹配同一客户端
		parsed, parseErr := jwt.ParseWithClaims(token.TokenID, &Claims{}, func(t *jwt.Token) (interface{}, error) {
			return s.secret, nil
		})
		if parseErr != nil {
			continue
		}
		claims, ok := parsed.Claims.(*Claims)
		if !ok || claims.ClientID != clientID {
			continue
		}
		if err := s.tokens.Revoke(ctx, token.TokenID, clientID, "logout"); err != nil {
			return fmt.Errorf("failed to revoke refresh token: %w", err)
		}
		// 清除缓存
		s.deleteTokenCache(token.TokenID)
	}

	return nil
}

// RefreshToken 刷新Token
func (s *AuthService) RefreshToken(ctx context.Context, refreshToken, clientInfo string) (*RefreshResponse, error) {
	// 验证Refresh Token
	isRevoked, err := s.tokens.IsRevoked(ctx, refreshToken)
	if err != nil {
		return nil, fmt.Errorf("failed to check token status: %w", err)
	}

	if isRevoked {
		return nil, fmt.Errorf("refresh token has been revoked")
	}

	// 获取Token详情
	token, err := s.tokens.GetByID(ctx, refreshToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	// 检查Token类型
	if token.TokenType != "refresh" {
		return nil, fmt.Errorf("invalid token type")
	}

	// 检查是否过期
	if token.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("refresh token has expired")
	}

	// 撤销旧的 Token 对
	if err := s.tokens.Revoke(ctx, refreshToken, "system", "token refreshed"); err != nil {
		return nil, fmt.Errorf("failed to revoke refresh token: %w", err)
	}
	// 清除旧 Token 的缓存
	s.deleteTokenCache(refreshToken)

	// 生成新的 Access Token (7 天过期)
	newAccessToken, accessExp, err := s.generateToken(token.ClientInfo, "access", 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new access token: %w", err)
	}

	// 生成新的 Refresh Token (30 天过期)
	newRefreshToken, refreshExp, err := s.generateToken(token.ClientInfo, "refresh", 30*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new refresh token: %w", err)
	}

	// 保存新 Token 到数据库
	now := time.Now()
	accessRecord := &models.AuthToken{
		TokenID:    newAccessToken,
		TokenType:  "access",
		ClientInfo: clientInfo,
		ExpiresAt:  accessExp,
		CreatedAt:  now,
	}

	refreshRecord := &models.AuthToken{
		TokenID:    newRefreshToken,
		TokenType:  "refresh",
		ClientInfo: clientInfo,
		ExpiresAt:  refreshExp,
		CreatedAt:  now,
	}

	if err := s.tokens.Create(ctx, accessRecord); err != nil {
		return nil, fmt.Errorf("failed to save new access token: %w", err)
	}

	if err := s.tokens.Create(ctx, refreshRecord); err != nil {
		return nil, fmt.Errorf("failed to save new refresh token: %w", err)
	}

	return &RefreshResponse{
		AccessToken:  newAccessToken,
		RefreshToken: newRefreshToken,
		ExpiresIn:    int64(accessExp.Sub(now).Seconds()),
		TokenType:    "Bearer",
	}, nil
}

// ValidateToken 验证 Token
func (s *AuthService) ValidateToken(ctx context.Context, tokenString string) (*Claims, error) {
	// 永久令牌不缓存认证结果，每个请求读取数据库以保证撤销立即生效。
	if strings.HasPrefix(tokenString, PersonalTokenPrefix) {
		return s.validatePersonalToken(ctx, tokenString)
	}
	// 先解析签名与过期时间。插件/系统票据不查缓存：撤销必须立刻生效，
	// 且代次校验是内存读取，成本可接受。
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return s.secret, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	switch claims.Kind {
	case KindPlugin:
		if err := s.validatePluginCredential(ctx, claims); err != nil {
			return nil, err
		}
		return claims, nil
	case KindSystem:
		return claims, nil
	}

	// 历史 plugin-system 票据（百年签名、跳过 DB 撤销）只在持久的兼容窗口内
	// 被接受，且降级为最小插件 scope；窗口结束即拒绝。新票据一律走代次模型。
	if claims.ClientID == "plugin-system" {
		if err := s.legacyPluginWindowOpen(ctx); err != nil {
			return nil, err
		}
		claims.Kind = KindPlugin
		claims.Legacy = true
		claims.ClientID = "plugin:legacy"
		claims.Scopes = plugincred.HTTPPluginScopes
		return claims, nil
	}

	// 用户票据先查缓存。
	if cacheEntry, found := s.getCachedToken(tokenString); found {
		return cacheEntry.Claims, nil
	}

	// 对于普通用户 Token，检查是否被撤销
	record, err := s.tokens.GetByID(ctx, tokenString)
	if err != nil {
		return nil, fmt.Errorf("failed to check token status: %w", err)
	}

	if !record.RevokedAt.IsZero() || record.ExpiresAt.Before(time.Now()) {
		// 缓存撤销状态，使用 token 的过期时间
		s.setTokenCache(tokenString, claims, claims.ExpiresAt.Time, true)
		return nil, fmt.Errorf("token has been revoked")
	}
	claims.CredentialType = record.TokenType

	// 缓存有效 Token，使用 token 的过期时间
	s.setTokenCache(tokenString, claims, claims.ExpiresAt.Time, false)

	return claims, nil
}

// ListActiveTokens 列出活跃Token
func (s *AuthService) ListActiveTokens(ctx context.Context, filter *database.TokenFilter) ([]*models.AuthToken, error) {
	return s.tokens.ListActive(ctx, filter)
}

// RevokeToken 撤销 Token
func (s *AuthService) RevokeToken(ctx context.Context, tokenID, revokedBy, reason string) error {
	err := s.tokens.Revoke(ctx, tokenID, revokedBy, reason)
	if err == nil {
		// 清除缓存
		s.deleteTokenCache(tokenID)
	}
	return err
}

// generateToken 生成JWT Token
func (s *AuthService) generateToken(clientID, tokenType string, expiresIn time.Duration) (string, time.Time, error) {
	expirationTime := time.Now().Add(expiresIn)

	tokenID, err := generateRandomString(32)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate token ID: %w", err)
	}

	claims := &Claims{
		ClientID: clientID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationTime),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        tokenID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, err
	}

	return tokenString, expirationTime, nil
}

// generateClientID 生成客户端ID
func generateClientID() (string, error) {
	return generateRandomString(16)
}

// generateRandomString 生成随机字符串
func generateRandomString(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return hex.EncodeToString(bytes)[:length], nil
}
