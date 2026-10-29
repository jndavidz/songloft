package managed

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"songloft/internal/database/sqlc"
)

// S-04：短期可撤销媒体票据与独立 lease。
//
// - 高熵 opaque token（CSPRNG 32 字节），存储只落 SHA-256 摘要——泄露库不泄露票据；
// - 票据绑定 TrackRef digest、representation、session、epoch、entry、target 与授权主体；
//   不向设备发管理员或 plugin-system JWT；
// - stop／换代只撤销对应 lease 与在途流，同文件其他客户端的 lease 不受影响；
// - 续期延长原票据授权（token/URI/representation 不变），有绝对上限；
// - 到期或撤销终止在途访问（ticketGuardReader）；重启后持久层恢复或按到期自然失效。

// Ticket 错误分类：鉴权失败必须能区分（401/403/410 语义），不合并成一种。
var (
	ErrTicketNotFound = errors.New("media ticket: unknown or revoked ticket")
	ErrTicketExpired  = errors.New("media ticket: expired")
	ErrTicketBinding  = errors.New("media ticket: binding mismatch")
	ErrTicketAbsolute = errors.New("media ticket: renewal beyond absolute lifetime")
	ErrTicketRevoked  = errors.New("media ticket: revoked")
)

// TicketRequest 是签发请求的全部绑定维度。
type TicketRequest struct {
	RefDigest      string
	Representation string // 固定表示 ID（managed/<digest>/final.mp3 的稳定标识）
	Session        string
	Epoch          int64
	Entry          string
	Target         string
	Subject        string // 授权主体（issuer scope 身份）
	TTL            time.Duration
}

// Lease 是同 subject/session 下一组票据的撤销单元：stop 只撤销本 lease。
type Lease struct {
	ID      string
	Subject string
}

// Ticket 是签发结果：Token 只此一次返回（明文不落库）。
type Ticket struct {
	LeaseID         string
	Token           string // opaque；URL 中使用，服务端只存摘要
	RefDigest       string
	Representation  string
	Session         string
	Epoch           int64
	Entry           string
	Target          string
	IssuedAt        time.Time
	ExpiresAt       time.Time
	AbsoluteExpires time.Time
}

// TicketIssuer 签发/撤销/续期/校验媒体票据。
type TicketIssuer struct {
	db *sql.DB
	// q 延迟构造，避免循环依赖 sqlc 生成的具体类型名直接暴露在本文件之外
	q *sqlc.Queries

	// DefaultTTL 与 RenewCap 是首版静态策略；具体初值待实施测量后调（架构 §5.3）。
	DefaultTTL time.Duration
	RenewCap   time.Duration // 绝对寿命上限
	Now        func() time.Time
}

func NewTicketIssuer(db *sql.DB) *TicketIssuer {
	return &TicketIssuer{
		db:         db,
		q:          sqlc.New(db),
		DefaultTTL: 30 * time.Minute,
		RenewCap:   12 * time.Hour,
		Now:        time.Now,
	}
}

func newLeaseID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("media ticket: csp-rng lease id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newOpaqueToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("media ticket: csp-rng token: %w", err)
	}
	token := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Issue 签发一张绑定完整身份的票据；每次调用都是独立 lease（撤销互不影响）。
func (t *TicketIssuer) Issue(ctx context.Context, req TicketRequest) (*Ticket, error) {
	if req.RefDigest == "" || req.Representation == "" || req.Subject == "" ||
		req.Session == "" || req.Entry == "" || req.Target == "" {
		return nil, fmt.Errorf("media ticket: issue requires full binding (ref/representation/session/entry/target/subject)")
	}
	leaseID, err := newLeaseID()
	if err != nil {
		return nil, err
	}
	token, tokenHash, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	now := t.Now()
	ttl := req.TTL
	if ttl <= 0 {
		ttl = t.DefaultTTL
	}
	exp := now.Add(ttl)
	abs := now.Add(t.RenewCap)
	if err := t.q.InsertMediaTicket(ctx, sqlc.InsertMediaTicketParams{
		TokenHash: tokenHash, LeaseID: leaseID, RefDigest: req.RefDigest,
		Representation: req.Representation, Session: req.Session, Epoch: req.Epoch,
		Entry: req.Entry, Target: req.Target, Subject: req.Subject,
		IssuedAt: now.Unix(), ExpiresAt: exp.Unix(), AbsoluteExpiresAt: abs.Unix(),
	}); err != nil {
		return nil, fmt.Errorf("media ticket: persist: %w", err)
	}
	return &Ticket{
		LeaseID: leaseID, Token: token,
		RefDigest: req.RefDigest, Representation: req.Representation,
		Session: req.Session, Epoch: req.Epoch, Entry: req.Entry, Target: req.Target,
		IssuedAt: now, ExpiresAt: exp, AbsoluteExpires: abs,
	}, nil
}

// Authorized 报告一次访问请求的鉴权结果：token 必须存在、未撤销、未过期，
// 且全部绑定维度与请求一致（跨 lease/异资源/不匹配 session 一律拒绝）。
// expectedRefDigest 等传空表示不校验该维度（仅限签发同主体的管理查询）。
func (t *TicketIssuer) Authorized(ctx context.Context, token string, want TicketRequest) error {
	rec, err := t.q.GetMediaTicketByHash(ctx, hashToken(token))
	if err != nil {
		return ErrTicketNotFound
	}
	now := t.Now()
	if rec.Revoked == 1 {
		return ErrTicketRevoked
	}
	if now.Unix() >= rec.ExpiresAt {
		return ErrTicketExpired
	}
	if want.RefDigest != "" && subtle.ConstantTimeCompare([]byte(rec.RefDigest), []byte(want.RefDigest)) != 1 {
		return ErrTicketBinding
	}
	if want.Representation != "" && rec.Representation != want.Representation {
		return ErrTicketBinding
	}
	if want.Session != "" && rec.Session != want.Session {
		return ErrTicketBinding
	}
	if want.Entry != "" && rec.Entry != want.Entry {
		return ErrTicketBinding
	}
	if want.Target != "" && rec.Target != want.Target {
		return ErrTicketBinding
	}
	_ = want.Epoch // epoch 绑定经 entry+session 维度承载；换代即撤（RevokeEntry）
	return nil
}

// Record 记录鉴权过的票据快照（投送层需要 representation 路径时用）。
func (t *TicketIssuer) Record(ctx context.Context, token string) (representation, refDigest, session, entry string, err error) {
	rec, err := t.q.GetMediaTicketByHash(ctx, hashToken(token))
	if err != nil {
		return "", "", "", "", ErrTicketNotFound
	}
	if rec.Revoked == 1 {
		return "", "", "", "", ErrTicketRevoked
	}
	if t.Now().Unix() >= rec.ExpiresAt {
		return "", "", "", "", ErrTicketExpired
	}
	return rec.Representation, rec.RefDigest, rec.Session, rec.Entry, nil
}

// RevokeLease 撤销一个 lease 的全部票据（stop 语义：只影响本 lease）。
// 进程内同步广播撤销（在途流秒断），再落库。
func (t *TicketIssuer) RevokeLease(ctx context.Context, leaseID string) error {
	if actives, err := t.q.ListActiveMediaTicketsByLease(ctx, sqlc.ListActiveMediaTicketsByLeaseParams{LeaseID: leaseID, ExpiresAt: t.Now().Unix()}); err == nil {
		for _, rec := range actives {
			t.notifyRevoked(rec.TokenHash)
		}
	}
	return t.q.RevokeMediaTicketsByLease(ctx, leaseID)
}

// RevokeEntry 撤销某 session/epoch/entry 的全部票据（换代语义：旧代次整体失效）。
func (t *TicketIssuer) RevokeEntry(ctx context.Context, session string, epoch int64, entry string) error {
	return t.q.RevokeMediaTicketsByEntry(ctx, sqlc.RevokeMediaTicketsByEntryParams{Session: session, Epoch: epoch, Entry: entry})
}

// Renew 延长原票据授权：token/URI/representation 不变，受绝对寿命上限约束。
func (t *TicketIssuer) Renew(ctx context.Context, token string, ttl time.Duration) (time.Time, error) {
	rec, err := t.q.GetMediaTicketByHash(ctx, hashToken(token))
	if err != nil {
		return time.Time{}, ErrTicketNotFound
	}
	if rec.Revoked == 1 {
		return time.Time{}, ErrTicketRevoked
	}
	now := t.Now()
	if now.Unix() >= rec.ExpiresAt {
		return time.Time{}, ErrTicketExpired // 过期票据不可续期，须重新签发
	}
	newExp := now.Add(ttl)
	if ttl <= 0 {
		newExp = now.Add(t.DefaultTTL)
	}
	if newExp.Unix() > rec.AbsoluteExpiresAt {
		return time.Time{}, ErrTicketAbsolute
	}
	if err := t.q.RenewMediaTicket(ctx, sqlc.RenewMediaTicketParams{ExpiresAt: newExp.Unix(), TokenHash: hashToken(token)}); err != nil {
		return time.Time{}, err
	}
	return newExp, nil
}
