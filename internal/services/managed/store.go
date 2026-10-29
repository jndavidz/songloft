package managed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"songloft/internal/database/sqlc"
)

// StateStore 把准备操作状态持久化（S-03：remote 生命周期首版只覆盖准备状态与恢复）。
// SQLiteStateStore 是生产实现；测试可用 nil Store（纯内存）。

// StateStore 是准备操作持久化的最小接口。
type StateStore interface {
	// Insert 在操作创建时落一行（state=preparing）。
	Insert(ctx context.Context, op *PrepareOp) error
	// Update 在状态/事实变化时更新对应行。
	Update(ctx context.Context, op *PrepareOp) error
	// ListActive 返回所有 state=preparing 的行（重启恢复用）。
	ListActive(ctx context.Context) ([]*PrepareOp, error)
}

// SQLiteStateStore 基于 sqlc 生成的 managed_prepare_ops 查询。
type SQLiteStateStore struct {
	q *sqlc.Queries
}

func NewSQLiteStateStore(db *sql.DB) *SQLiteStateStore {
	return &SQLiteStateStore{q: sqlc.New(db)}
}

func (s *SQLiteStateStore) Insert(ctx context.Context, op *PrepareOp) error {
	return s.q.InsertManagedPrepareOp(ctx, sqlc.InsertManagedPrepareOpParams{
		Issuer: op.Issuer, Session: op.Key.Session, Epoch: op.Key.Epoch,
		Entry: op.Key.Entry, RefDigest: op.RefDigest, State: string(op.State),
		Error: op.Error, ResolvedSource: op.ResolvedSource,
		FallbackReason: op.FallbackReason,
		CreatedAt:      op.CreatedAt.Unix(), UpdatedAt: op.UpdatedAt.Unix(),
	})
}

func (s *SQLiteStateStore) Update(ctx context.Context, op *PrepareOp) error {
	return s.q.UpdateManagedPrepareOpState(ctx, sqlc.UpdateManagedPrepareOpStateParams{
		State: string(op.State), Error: op.Error,
		ResolvedSource: op.ResolvedSource, FallbackReason: op.FallbackReason,
		UpdatedAt: time.Now().Unix(),
		Issuer:    op.Issuer, Session: op.Key.Session, Epoch: op.Key.Epoch, Entry: op.Key.Entry,
	})
}

func (s *SQLiteStateStore) ListActive(ctx context.Context) ([]*PrepareOp, error) {
	rows, err := s.q.ListActiveManagedPrepareOps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*PrepareOp, 0, len(rows))
	for _, r := range rows {
		out = append(out, &PrepareOp{
			Key:    OpKey{Session: r.Session, Epoch: r.Epoch, Entry: r.Entry},
			Issuer: r.Issuer, RefDigest: r.RefDigest,
			State: OpState(r.State), Error: r.Error,
			ResolvedSource: r.ResolvedSource, FallbackReason: r.FallbackReason,
			CreatedAt: time.Unix(r.CreatedAt, 0), UpdatedAt: time.Unix(r.UpdatedAt, 0),
		})
	}
	return out, nil
}

// persist 异步落库：持久化失败不阻断准备管线（恢复语义由 ListActive+interrupted 兜底），
// 但错误必须可见——记入 op.Error 不合适（会污染业务错误），仅依赖调用方日志。
func (m *Manager) persist(op *PrepareOp) {
	if m.store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.store.Update(ctx, op); err != nil {
			_ = err // 持久化失败不回滚状态机；恢复时 ListActive 只认 preparing 行
		}
	}()
}

// RefFromSourceData 把插件 source_data（kwqq 形态 {"s","i","c",...}）构造为
// TrackRef。c 字段只在 migu 源有身份意义（契约：copyrightId，kwqq 源会被
// 白名单丢弃）；bilibili 需 cid/page，source_data 缺失时明确失败——
// 「缺身份明确失败，不静默错配」。sources 非空时同时校验注册表。
func RefFromSourceData(sourceData string, sources []string) (*Ref, error) {
	var sd struct {
		S string `json:"s"`
		I any    `json:"i"`
		C any    `json:"c"`
	}
	if err := json.Unmarshal([]byte(sourceData), &sd); err != nil {
		return nil, fmt.Errorf("managed: source_data is not a valid identity payload: %w", err)
	}
	extra := map[string]any{}
	if v := strings.TrimSpace(toStr(sd.C)); v != "" {
		extra["copyrightId"] = v
	}
	return MakeRef(sd.S, toStr(sd.I), extra, sources)
}

// WithExtra 合并额外身份字段后重新校验——B站 cid/page 的选中时补全入口
// （kwqq /song/info 回传 extra.cid/page 后调用）。合并后身份 digest 变化
// 由 MakeRef 重新计算，不允许绕过校验直接改 Extra。
func (r *Ref) WithExtra(extra map[string]any) (*Ref, error) {
	if r == nil {
		return nil, refErr("nil ref")
	}
	merged := map[string]any{}
	for k, v := range r.Extra {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return MakeRef(r.Source, r.ID, merged, nil)
}
