package database

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"songloft/internal/database/sqlc"
)

// PlayHistoryRow 是播放历史的一条原始记录（不含歌曲详情，由 service 层水合）。
type PlayHistoryRow struct {
	SongID    int64
	PlayedAt  time.Time
	PlayCount int
}

// PlayHistoryRepository 负责 play_history 表的读写。
//
// 「播放上下文」由 (context_type, context_key) 二元组标识：歌单存
// ("playlist", "<id>")，分面维度存 ("artist", "周杰伦") 这类。
type PlayHistoryRepository struct {
	db      sqlc.DBTX
	queries *sqlc.Queries
}

// NewPlayHistoryRepository 创建仓储实例。
func NewPlayHistoryRepository(db sqlc.DBTX) *PlayHistoryRepository {
	return &PlayHistoryRepository{db: db, queries: sqlc.New(db)}
}

// Record 记录一次播放：同一上下文内的同一首歌只保留一行，
// 重复播放刷新 played_at 并递增 play_count（靠 UNIQUE 约束 upsert，不做应用层查重）。
func (r *PlayHistoryRepository) Record(ctx context.Context, contextType, contextKey string, songID int64, playedAt time.Time) error {
	if err := r.queries.RecordPlay(ctx, sqlc.RecordPlayParams{
		ContextType: contextType,
		ContextKey:  contextKey,
		SongID:      songID,
		PlayedAt:    playedAt,
	}); err != nil {
		return fmt.Errorf("record play history: %w", err)
	}
	return nil
}

// Trim 裁剪掉该上下文中超出 keep 条的最旧记录。
// keep <= 0 时不做任何事（避免误清空整个上下文）。
func (r *PlayHistoryRepository) Trim(ctx context.Context, contextType, contextKey string, keep int) error {
	if keep <= 0 {
		return nil
	}
	if err := r.queries.TrimPlayHistory(ctx, sqlc.TrimPlayHistoryParams{
		ContextType:   contextType,
		ContextKey:    contextKey,
		ContextType_2: contextType,
		ContextKey_2:  contextKey,
		Limit:         int64(keep),
	}); err != nil {
		return fmt.Errorf("trim play history: %w", err)
	}
	return nil
}

// List 按 played_at 倒序返回该上下文最近的播放记录。
func (r *PlayHistoryRepository) List(ctx context.Context, contextType, contextKey string, limit int) ([]PlayHistoryRow, error) {
	if limit <= 0 {
		return []PlayHistoryRow{}, nil
	}
	rows, err := r.queries.ListPlayHistory(ctx, sqlc.ListPlayHistoryParams{
		ContextType: contextType,
		ContextKey:  contextKey,
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list play history: %w", err)
	}
	out := make([]PlayHistoryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, PlayHistoryRow{
			SongID:    row.SongID,
			PlayedAt:  row.PlayedAt,
			PlayCount: int(row.PlayCount),
		})
	}
	return out, nil
}

// Count 返回该上下文的历史记录总数。
func (r *PlayHistoryRepository) Count(ctx context.Context, contextType, contextKey string) (int, error) {
	n, err := r.queries.CountPlayHistory(ctx, sqlc.CountPlayHistoryParams{
		ContextType: contextType,
		ContextKey:  contextKey,
	})
	if err != nil {
		return 0, fmt.Errorf("count play history: %w", err)
	}
	return int(n), nil
}

// ListRecent 跨上下文返回最近播放记录：按歌曲去重（同一首歌取最近一次），按播放时间倒序。
// 极小概率下同一首歌在两个上下文的 played_at 完全相同会产生重复行，由调用方按 song_id 去重。
func (r *PlayHistoryRepository) ListRecent(ctx context.Context, limit, offset int) ([]PlayHistoryRow, error) {
	if limit <= 0 {
		return []PlayHistoryRow{}, nil
	}
	rows, err := r.queries.ListRecentPlayHistory(ctx, sqlc.ListRecentPlayHistoryParams{
		Limit:  int64(limit),
		Offset: int64(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list recent play history: %w", err)
	}
	out := make([]PlayHistoryRow, 0, len(rows))
	seen := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		if _, dup := seen[row.SongID]; dup {
			continue
		}
		seen[row.SongID] = struct{}{}
		out = append(out, PlayHistoryRow{
			SongID:    row.SongID,
			PlayedAt:  row.PlayedAt,
			PlayCount: int(row.PlayCount),
		})
	}
	return out, nil
}

// CountRecent 返回跨上下文去重后的歌曲数。
func (r *PlayHistoryRepository) CountRecent(ctx context.Context) (int, error) {
	n, err := r.queries.CountRecentPlayHistory(ctx)
	if err != nil {
		return 0, fmt.Errorf("count recent play history: %w", err)
	}
	return int(n), nil
}

// Clear 清空该上下文的全部历史，返回删除行数。
func (r *PlayHistoryRepository) Clear(ctx context.Context, contextType, contextKey string) (int, error) {
	rows, err := r.queries.ClearPlayHistory(ctx, sqlc.ClearPlayHistoryParams{
		ContextType: contextType,
		ContextKey:  contextKey,
	})
	if err != nil {
		return 0, fmt.Errorf("clear play history: %w", err)
	}
	return int(rows), nil
}

// DeleteEntry 删除单条历史记录，未命中时返回 ErrNotFound。
func (r *PlayHistoryRepository) DeleteEntry(ctx context.Context, contextType, contextKey string, songID int64) error {
	rows, err := r.queries.DeletePlayHistoryEntry(ctx, sqlc.DeletePlayHistoryEntryParams{
		ContextType: contextType,
		ContextKey:  contextKey,
		SongID:      songID,
	})
	if err != nil {
		return fmt.Errorf("delete play history entry: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearByPlaylist 清理某歌单的播放历史。
// context_key 是 TEXT，无法对 playlists 建外键做级联，故删歌单时必须显式调用本方法。
func (r *PlayHistoryRepository) ClearByPlaylist(ctx context.Context, playlistID int64) (int, error) {
	rows, err := r.queries.ClearPlayHistoryByPlaylist(ctx, strconv.FormatInt(playlistID, 10))
	if err != nil {
		return 0, fmt.Errorf("clear play history by playlist: %w", err)
	}
	return int(rows), nil
}

// ClearByTag 清理某自定义标签的播放历史。
// 与歌单同理：context_key 是 TEXT，无法对 song_tags 建外键做级联，删标签时必须显式调用。
func (r *PlayHistoryRepository) ClearByTag(ctx context.Context, tagID int64) (int, error) {
	rows, err := r.queries.ClearPlayHistoryByTag(ctx, strconv.FormatInt(tagID, 10))
	if err != nil {
		return 0, fmt.Errorf("clear play history by tag: %w", err)
	}
	return int(rows), nil
}

// ClearAll 清空全部上下文的播放历史，返回删除行数。
func (r *PlayHistoryRepository) ClearAll(ctx context.Context) (int, error) {
	rows, err := r.queries.ClearAllPlayHistory(ctx)
	if err != nil {
		return 0, fmt.Errorf("clear all play history: %w", err)
	}
	return int(rows), nil
}
