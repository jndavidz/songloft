package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"songloft/internal/plugincred"
	"songloft/internal/services"
	"songloft/internal/services/requestidentity"
)

// contextKey 是 context value key 的专用类型，避免跨包碰撞。
type contextKey string

const credentialTypeKey contextKey = "credential_type"

// RequireLoginSession 限制凭证管理仅由正常登录的访问令牌操作。
func RequireLoginSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(credentialTypeKey) != "access" {
			respondAuthError(w, http.StatusForbidden, "请通过账号登录后管理 Token", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

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

			// 插件票据：按 manifest scope 做路由级授权（S-02）。管理面、
			// 鉴权面与未归类路径一律拒绝；插件只能访问自己的子路径。
			if claims.Kind == services.KindPlugin {
				if err := authorizePluginRequest(claims, r); err != nil {
					respondAuthError(w, http.StatusForbidden, "插件凭据权限不足", err)
					return
				}
			}

			var ctx context.Context
			if claims.Kind == services.KindSystem {
				// 系统票据是宿主内部调用凭据（URL resolver 等），只接受回环来源。
				// 它不是任何外部消费者的通行证，被带到外部请求上必须拒绝。
				if !isLoopbackRequest(r) {
					respondAuthError(w, http.StatusUnauthorized, "系统票据仅限本机调用", nil)
					return
				}
				ctx = requestidentity.SystemContext(r.Context())
			} else {
				// 身份统一经 requestidentity 写入（本中间件是唯一写入方）。
				// 真实认证请求缺主体（client_id 为空）必须失败关闭，
				// 不得产出会落入共享零值桶的身份（S-01）。
				identity, err := requestidentity.User(claims.ClientID)
				if err != nil {
					respondAuthError(w, http.StatusUnauthorized, "token 缺少 client_id", nil)
					return
				}
				ctx = requestidentity.WithIdentity(r.Context(), identity)
			}
			// 凭证类别由签发记录决定，供 RequireLoginSession 拒绝永久令牌与插件票据。
			ctx = context.WithValue(ctx, credentialTypeKey, claims.CredentialType)

			// 认证成功，继续处理请求
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// authorizePluginRequest 按插件票据 scope 判定本次请求是否允许。
//
// 语义：scope 精确匹配（签发侧已把 manifest 通配符展开）；无法归类的路径
// 拒绝而不是放行——插件票据永不该触达管理面。
func authorizePluginRequest(claims *services.Claims, r *http.Request) error {
	if entry, ok := plugincred.PluginSelfEntry(r.URL.Path); ok {
		// 旧 plugin-system 票据没有插件主体，窗口内保留旧行为（可访问任意插件
		// 自身路径）；新票据只能访问自己的 entry。
		if claims.Legacy {
			return nil
		}
		if claims.PluginEntry == "" || entry != claims.PluginEntry {
			return fmt.Errorf("plugin %q cannot access %q", claims.PluginEntry, r.URL.Path)
		}
		return nil
	}

	required, ok := plugincred.RequiredScope(r.Method, r.URL.Path)
	if !ok {
		return fmt.Errorf("path %q is outside plugin credential scope", r.URL.Path)
	}
	if !plugincred.HasScope(claims.Scopes, required) {
		return fmt.Errorf("missing scope %q", required)
	}
	return nil
}

// isLoopbackRequest 判断请求是否来自回环地址。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
