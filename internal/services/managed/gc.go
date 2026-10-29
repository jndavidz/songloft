package managed

import (
	"context"
	"database/sql"
	"time"

	"songloft/internal/database/sqlc"
)

// S-09：非破坏性 remote 隔离与 GC 预览。
//
// 首版只做可恢复隔离（tombstone），绝不物理删除；本文件的分析器是纯函数
// ——排除矩阵表驱动（收藏/歌单/活动队列/lease/准备中/近期失败重试/显式
// 下载/听音事件引用），与曲库存储解耦，误删负例在测试里锁定。
// legacy remote 不因「播放过一次」或「同名」被清理——排除判定只看引用
// 与状态，不看名称或播放历史计数。

// GCSong 候选考察对象。
type GCSong struct {
	ID        int64
	RefDigest string
	CachePath string // 非空 = 显式下载/已缓存表示（排除）
}

// GCReferences 一个 song 的全部引用证据。
type GCReferences struct {
	InPlaylist      bool // 任何歌单（含收藏歌单）引用
	InActiveQueue   bool // 活动托管队列引用
	HasLease        bool // 有效媒体票据/lease 引用
	Preparing       bool // 准备中
	RecentFailure   bool // 近期失败重试窗口内
	ExplicitDl      bool // 显式下载
	InListenHistory bool // 听音事件引用
}

// Referenced 报告该 song 是否被任何排除条件覆盖。
func (r GCReferences) Referenced() bool {
	return r.InPlaylist || r.InActiveQueue || r.HasLease || r.Preparing ||
		r.RecentFailure || r.ExplicitDl || r.InListenHistory
}

// GCCandidate 隔离候选（审计清单条目）。
type GCCandidate struct {
	SongID    int64  `json:"song_id"`
	RefDigest string `json:"ref_digest"`
	CachePath string `json:"cache_path,omitempty"`
}

// AnalyzeOrphans 纯分析：返回「零引用且无显式缓存」的 remote 候选。
// 输入不含 tombstone——已隔离 songs 由调用方预先剔除（幂等由 tombstone
// 表的 ON CONFLICT DO NOTHING 双保险）。prevReleaseExempt 是 legacy remote
// 的保护名单（如发布前已存在的旧数据），本函数绝不越过它。
func AnalyzeOrphans(songs []GCSong, refs map[int64]GCReferences, prevReleaseExempt map[int64]bool) []GCCandidate {
	out := make([]GCCandidate, 0)
	for _, s := range songs {
		if prevReleaseExempt[s.ID] {
			continue
		}
		if refs[s.ID].Referenced() {
			continue
		}
		if s.CachePath != "" {
			continue // 显式下载/已缓存表示，零触碰
		}
		out = append(out, GCCandidate{SongID: s.ID, RefDigest: s.RefDigest})
	}
	return out
}

// TombstoneStore 隔离标记持久化：预览/隔离/恢复全程可审计，无删除。
type TombstoneStore struct {
	q *sqlc.Queries
}

func NewTombstoneStore(db *sql.DB) *TombstoneStore {
	return &TombstoneStore{q: sqlc.New(db)}
}

// Quarantine 隔离候选（幂等：已隔离的 song 不重复写行）。
func (s *TombstoneStore) Quarantine(ctx context.Context, c GCCandidate, reason string) error {
	return s.q.InsertGCTombstone(ctx, sqlc.InsertGCTombstoneParams{
		SongID: c.SongID, RefDigest: c.RefDigest, Reason: reason,
		QuarantinedAt: time.Now().Unix(),
	})
}

// QuarantinedIDs 当前隔离中的 song（重复运行与排除判定用）。
func (s *TombstoneStore) QuarantinedIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.q.ListGCTombstonedSongIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(rows))
	for _, id := range rows {
		out[id] = true
	}
	return out, nil
}

// Restore 解除隔离（可恢复隔离的另一半）。
func (s *TombstoneStore) Restore(ctx context.Context, songID int64) error {
	return s.q.RestoreGCTombstone(ctx, sqlc.RestoreGCTombstoneParams{RestoredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}, SongID: songID})
}

// Count 当前隔离数（审计）。
func (s *TombstoneStore) Count(ctx context.Context) (int64, error) {
	return s.q.CountGCTombstones(ctx)
}
