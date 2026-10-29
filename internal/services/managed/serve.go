package managed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 票据媒体服务（S-04/S-05）：只服务已 ready 的固定表示。
// HEAD 无响应体且不触发解析/下载/ffmpeg（文件已存在，纯读盘）；
// GET/Range/If-Range 语义由 http.ServeContent 按 RFC 9110 实现；
// 在途终止经 ticketGuardReader：到期即停、撤销秒级停。

// MediaServer 服务票据化的固定表示。
type MediaServer struct {
	Issuer *TicketIssuer
	// Root 是表示隔离根（Manager 的 WorkDir）：representation 记录形如
	// "managed/<digest>/final.mp3" 的相对路径，拼接前做穿越校验。
	Root string
	now  func() time.Time
}

func NewMediaServer(issuer *TicketIssuer, root string) *MediaServer {
	return &MediaServer{Issuer: issuer, Root: root, now: issuer.Now}
}

// RevocationRegistry 是进程内撤销广播：Revoke 后秒级终止在途流，
// 不等下一次 DB 轮询。
type RevocationRegistry struct {
	mu      sync.Mutex
	revoked map[string]struct{}
}

var globalRevocations = &RevocationRegistry{revoked: map[string]struct{}{}}

func (r *RevocationRegistry) add(tokenHash string) {
	r.mu.Lock()
	r.revoked[tokenHash] = struct{}{}
	r.mu.Unlock()
}

func (r *RevocationRegistry) has(tokenHash string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.revoked[tokenHash]
	return ok
}

// guardReader 在每次 Read 时强制票据仍然有效：过期立即断流；
// 进程内撤销即时生效，DB 撤销一秒内生效。
type guardReader struct {
	rs        io.ReadSeeker
	iss       *TicketIssuer
	tokenHash string
	expiresAt time.Time
	lastCheck time.Time
}

func (g *guardReader) Read(p []byte) (int, error) {
	now := g.iss.Now()
	if !now.Before(g.expiresAt) {
		return 0, ErrTicketExpired
	}
	if now.Sub(g.lastCheck) >= time.Second {
		g.lastCheck = now
		// 进程内撤销广播即时生效；DB 撤销（换代/其他入口）一秒内生效
		if globalRevocations.has(g.tokenHash) {
			return 0, ErrTicketRevoked
		}
		if rec, err := g.iss.q.GetMediaTicketByHash(context.Background(), g.tokenHash); err == nil && rec.Revoked == 1 {
			return 0, ErrTicketRevoked
		}
	}
	return g.rs.Read(p)
}

func (g *guardReader) Seek(offset int64, whence int) (int64, error) {
	return g.rs.Seek(offset, whence)
}

// resolveRepresentationPath 把 representation 记录解析为 Root 下的真实路径，
// 拒绝任何穿越（representation 永远是 managed/<digest>/<name> 形态）。
func (s *MediaServer) resolveRepresentationPath(rep string) (string, error) {
	clean := path.Clean("/" + rep) // 以 / 锚定后 Clean，杀掉 ../
	rel := strings.TrimPrefix(clean, "/")
	if !strings.HasPrefix(rel, "managed/") {
		return "", errors.New("media serve: representation outside managed root")
	}
	return filepath.Join(s.Root, filepath.FromSlash(rel)), nil
}

// ServeMedia 是 /media 端点的 handler：票据鉴权 → 固定表示字节服务。
// 任何拒绝都走统一错误（不区分「票据不存在/已撤销」给调用方细节，防枚举）。
func (s *MediaServer) ServeMedia(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("media_ticket")
	if token == "" {
		http.Error(w, "missing media ticket", http.StatusUnauthorized)
		return
	}
	rep, _, _, _, err := s.Issuer.Record(r.Context(), token)
	if err != nil {
		switch {
		case errors.Is(err, ErrTicketExpired):
			http.Error(w, "media ticket expired", http.StatusUnauthorized)
		default:
			// 不存在/已撤销/绑定不符统一 401，不泄露具体原因（防票据枚举）
			http.Error(w, "invalid media ticket", http.StatusUnauthorized)
		}
		return
	}
	diskPath, err := s.resolveRepresentationPath(rep)
	if err != nil {
		http.Error(w, "invalid representation", http.StatusBadRequest)
		return
	}
	f, err := os.Open(diskPath)
	if err != nil {
		slog.Warn("media serve: open representation failed",
			"representation", RedactTicketParam(rep), "error", err)
		http.Error(w, "representation unavailable", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "representation unavailable", http.StatusNotFound)
		return
	}

	gr := &guardReader{
		rs:        f,
		iss:       s.Issuer,
		tokenHash: hashToken(token),
		expiresAt: s.Issuer.Now().Add(s.Issuer.DefaultTTL), // 真实 expiry 由 Record 已验；guard 用 DB 快照
		lastCheck: s.Issuer.Now(),
	}
	// guard 的 expiresAt 取票据真实到期时间：Record 已确认未过期，这里查库一次拿准。
	if rec, err := s.Issuer.q.GetMediaTicketByHash(r.Context(), hashToken(token)); err == nil {
		gr.expiresAt = time.Unix(rec.ExpiresAt, 0)
	}

	// ServeContent：GET/HEAD/Range/If-Range（RFC 9110），Content-Type 由扩展名推断。
	http.ServeContent(w, r, path.Base(diskPath), st.ModTime(), gr)
}

// RevokeHook 供 TicketIssuer 撤销时同步广播进程内撤销。
func (t *TicketIssuer) notifyRevoked(tokenHash string) {
	globalRevocations.add(tokenHash)
}

// RedactTicketParam 把 URL 中的票据参数替换为占位——日志/事件里禁止出现
// 可重放的票据 URL（「没有 query」不等于无泄露）。
func RedactTicketParam(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparsable]"
	}
	q := u.Query()
	redacted := false
	for key := range q {
		if key == "media_ticket" || strings.HasSuffix(key, "_ticket") {
			q.Set(key, "[redacted]")
			redacted = true
		}
	}
	if !redacted {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}
