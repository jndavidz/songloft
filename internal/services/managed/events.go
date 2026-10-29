package managed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"songloft/internal/database/sqlc"
)

// S-06：听音事件。事件携带 TrackRef digest、entry、lease 与幂等键；
// 重复幂等键不乱记历史；prepare/HEAD/Range/prefetch 不记已听；
// 崩溃或长期断连由有界 outbox + replay（seq 续传）补偿。

// 事件类型。Recordable 白名单之外的类型一律不作为「已听」历史。
const (
	EventPlayed   = "played"   // 真实播放开始（已听唯一入口）
	EventProgress = "progress" // 进度心跳
	EventComplete = "complete" // 自然完成
	EventError    = "error"    // 播放错误
)

// recordableTypes 允许进入历史/出箱的类型白名单——prepare、HEAD、Range、
// prefetch 等预听为不在列（ticket：不记已听）。
var recordableTypes = map[string]bool{
	EventPlayed: true, EventProgress: true, EventComplete: true, EventError: true,
}

// ListenEvent 单条听音事件。
type ListenEvent struct {
	Seq            int64          `json:"seq"`
	Type           string         `json:"type"`
	RefDigest      string         `json:"ref_digest"`
	Entry          string         `json:"entry"`
	LeaseID        string         `json:"lease_id"`
	Session        string         `json:"session"`
	IdempotencyKey string         `json:"idempotency_key"`
	Payload        map[string]any `json:"payload,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// EventLog 有界听音事件日志。
type EventLog struct {
	q *sqlc.Queries
}

func NewEventLog(db *sql.DB) *EventLog { return &EventLog{q: sqlc.New(db)} }

// Append 记录事件；幂等键重复时返回既有 seq（重复投递不乱记历史）。
// 非 recordable 类型被显式拒绝——预听为不得经此进入已听历史。
func (l *EventLog) Append(ctx context.Context, ev *ListenEvent) (int64, error) {
	if !recordableTypes[ev.Type] {
		return 0, ErrNotRecordable
	}
	if ev.IdempotencyKey == "" {
		return 0, ErrIdempotencyRequired
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return 0, err
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	row, err := l.q.InsertListeningEvent(ctx, sqlc.InsertListeningEventParams{
		IdempotencyKey: ev.IdempotencyKey, Type: ev.Type,
		RefDigest: ev.RefDigest, Entry: ev.Entry, LeaseID: ev.LeaseID,
		Session: ev.Session, Payload: string(payload),
		CreatedAt: ev.CreatedAt.Unix(),
	})
	if err != nil {
		// ON CONFLICT DO NOTHING：冲突时 sqlc 无行返回（ErrNoRows）→ 取既有 seq
		if errors.Is(err, sql.ErrNoRows) {
			if seq, gerr := l.q.GetListeningEventSeq(ctx, ev.IdempotencyKey); gerr == nil {
				return seq, nil
			}
		}
		return 0, err
	}
	return row.Seq, nil
}

// Pending 返回 seq 之后未确认的事件（断连补偿的 replay 入口，Last-Event-ID 语义）。
func (l *EventLog) Pending(ctx context.Context, afterSeq int64, limit int) ([]*ListenEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := l.q.ListPendingListeningEvents(ctx, sqlc.ListPendingListeningEventsParams{
		Seq: afterSeq, Limit: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*ListenEvent, 0, len(rows))
	for _, r := range rows {
		ev, err := rowToEvent(r.Seq, r.IdempotencyKey, r.Type, r.RefDigest,
			r.Entry, r.LeaseID, r.Session, r.Payload, r.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// Ack 确认消费到 seq（消费方断点续传）。
func (l *EventLog) Ack(ctx context.Context, uptoSeq int64) error {
	if err := l.q.AckListeningEventsUpTo(ctx, uptoSeq); err != nil {
		return err
	}
	// 有界：已确认历史只保留最新 keep 条（补偿窗口）
	minSeq, err := l.q.MinAckedListeningEventSeq(ctx, 500)
	if err != nil {
		return err
	}
	if minSeq > 0 {
		return l.q.DeleteListeningEventsBelow(ctx, minSeq)
	}
	return nil
}

// MaxSeq 当前最大 seq（消费方从崩溃恢复时对齐起点）。
func (l *EventLog) MaxSeq(ctx context.Context) (int64, error) {
	v, err := l.q.MaxListeningEventSeq(ctx)
	if err != nil {
		return 0, err
	}
	switch t := v.(type) {
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	default:
		return 0, nil
	}
}

var (
	ErrNotRecordable       = &typedError{"managed: event type is not recordable as listening history"}
	ErrIdempotencyRequired = &typedError{"managed: event requires idempotency key"}
)

type typedError struct{ msg string }

func (e *typedError) Error() string { return e.msg }

func rowToEvent(seq int64, key, typ, refDigest, entry, leaseID, session, payload string, createdAt int64) (*ListenEvent, error) {
	var pl map[string]any
	if payload != "" && payload != "{}" {
		if err := json.Unmarshal([]byte(payload), &pl); err != nil {
			return nil, err
		}
	}
	return &ListenEvent{
		Seq: seq, Type: typ, RefDigest: refDigest, Entry: entry,
		LeaseID: leaseID, Session: session, IdempotencyKey: key,
		Payload: pl, CreatedAt: time.Unix(createdAt, 0),
	}, nil
}
