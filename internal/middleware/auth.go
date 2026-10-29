package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"songloft/internal/services"
	"songloft/internal/services/requestidentity"
)

// ClientIDFromContext 从请求上下文中提取认证主体（client_id）。
// 经 requestidentity 统一读取；仅认证后的用户身份有值，系统身份与缺身份返回空串。
func ClientIDFromContext(ctx context.Context) string {
	if id, ok := requestidentity.FromContext(ctx); ok && id.Kind == requestidentity.KindUser {
		return id.ClientID
	}
	return ""
}

func respondAuthError(w http.ResponseWriter, status int, message string, err error) {
	response := map[string]string{"error": message}
	if err != nil {
		response["detail"] = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("json encode failed", "error", err)
	}
}

// PublicPathChecker 用于检查请求路径是否为公开路径（无需 JWT）。
type PublicPathChecker interface {
	IsPublicPath(path string) bool
}

// AuthMiddleware 认证中间件
func AuthMiddleware(authService *services.AuthService, publicPathCheckers ...PublicPathChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 检查是否为公开路径（如插件 publicPaths 声明的 Subsonic /rest/* 端点）
			for _, checker := range publicPathCheckers {
				if checker != nil && checker.IsPublicPath(r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
			}

			var tokenString string

			// 优先从 Authorization 头获取 token
			authHeader := r.Header.Get("Authorization")
			if authHeader != "" {
				extracted := strings.TrimPrefix(authHeader, "Bearer ")
				if extracted != authHeader {
					tokenString = extracted
				}
			}

			// 回退：从 URL query parameter 获取 token
			// 用于图片/音频等无法自定义 Header 的场景（如 <img> 标签、CachedNetworkImage）
			if tokenString == "" {
				tokenString = r.URL.Query().Get("access_token")
				// 小爱音箱固件会将 URL 中的 & 替换为空格，导致后续参数被合并进 access_token。
				// JWT 不含空格，按空格拆分并将被吞掉的参数还原到 query string。
				if token, remainder, ok := strings.Cut(tokenString, " "); ok {
					tokenString = token
					q := r.URL.Query()
					q.Set("access_token", tokenString)
					for kv := range strings.SplitSeq(remainder, " ") {
						if k, v, found := strings.Cut(kv, "="); found {
							q.Set(k, v)
						}
					}
					r.URL.RawQuery = q.Encode()
				}
			}

			if tokenString == "" {
				respondAuthError(w, http.StatusUnauthorized, "缺少认证信息", nil)
				return
			}

			// 验证 JWT token
			claims, err := authService.ValidateToken(r.Context(), tokenString)
			if err != nil {
				respondAuthError(w, http.StatusUnauthorized, "无效的 token", err)
				return
			}

			// 身份统一经 requestidentity 写入（本中间件是唯一写入方）。
			// 真实认证请求缺主体（client_id 为空）必须失败关闭，
			// 不得产出会落入共享零值桶的身份（S-01）。
			identity, err := requestidentity.User(claims.ClientID)
			if err != nil {
				respondAuthError(w, http.StatusUnauthorized, "token 缺少 client_id", nil)
				return
			}
			ctx := requestidentity.WithIdentity(r.Context(), identity)

			// 认证成功，继续处理请求
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
