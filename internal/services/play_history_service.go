package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"songloft/internal/database"
	"songloft/internal/models"
)

// MaxPlayHistoryPerContext 单个播放上下文保留的历史条数上限。
// 刻意做成 Go 常量而非可配置项：50 条足够覆盖「上次听到哪」的诉求，
// 做成配置就得再开一个配置端点，收益不匹配。
const MaxPlayHistoryPerContext = 50

// ErrInvalidPlayContext 表示播放上下文的 type 或 key 不合法，handler 据此返回 400。
var ErrInvalidPlayContext = errors.New("invalid play context")

// IsValidPlayContextType 判断播放上下文类型是否受支持。
// 合法取值 = "playlist" + "tag" + "browse" + 全部歌曲分面维度，两处清单分别由 models 与 database 持有，
// 这里组合它们，避免再抄第三份枚举。
// "browse" 是客户端曲库浏览（插件音源）点播用的上下文（key 为音源标识），
// 客户端自 2026-11 起统一上报，缺它会导致这类播放被静默丢弃（接口仍回 204）。
func IsValidPlayContextType(contextType string) bool {
	return contextType == models.PlayContextPlaylist ||
		contextType == models.PlayContextTag ||
		contextType == models.PlayContextBrowse ||
		database.IsSongFacetField(contextType)
}

// PlayHistoryService 维护「每个播放上下文最近播放过哪些歌」。
//
// 上下文由 (type, key) 标识：歌单是 ("playlist", "<id>")，分面维度是
// ("artist", "周杰伦") 这类。用户切换歌单/歌手/专辑后可回到原上下文接着往下播。
//
// 这里接收 database.DB 而非单个 Repository：Record 需要把 upsert 与裁剪放进
// 同一个事务，按项目铁律只能走 db.RunInTx，service 层不得自行 BeginTx。
type PlayHistoryService struct {
	db database.DB
}

// NewPlayHistoryService 创建播放历史服务。
func NewPlayHistoryService(db database.DB) *PlayHistoryService {
	return &PlayHistoryService{db: db}
}

// Record 记录一次播放，并把该上下文裁剪到 MaxPlayHistoryPerContext 条。
// 两个写操作在同一事务内，避免中途失败留下超额记录。
func (s *PlayHistoryService) Record(ctx context.Context, contextType, contextKey string, songID int64, playedAt time.Time) error {
	if !IsValidPlayContextType(contextType) {
		return fmt.Errorf("%w: type %q", ErrInvalidPlayContext, contextType)
	}
	if contextKey == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidPlayContext)
	}
	return s.db.RunInTx(ctx, func(ctx context.Context, uow *database.UnitOfWork) error {
		if err := uow.PlayHistory.Record(ctx, contextType, contextKey, songID, playedAt); err != nil {
			return err
		}
		return uow.PlayHistory.Trim(ctx, contextType, contextKey, MaxPlayHistoryPerContext)
	})
}

// List 返回该上下文最近播放的歌曲（含完整歌曲详情），按播放时间倒序。
func (s *PlayHistoryService) List(ctx context.Context, contextType, contextKey string, limit int) ([]models.PlayHistoryEntry, error) {
	if !IsValidPlayContextType(contextType) {
		return nil, fmt.Errorf("%w: type %q", ErrInvalidPlayContext, contextType)
	}
	rows, err := s.db.PlayHistoryRepository().List(ctx, contextType, contextKey, limit)
	if err != nil {
		return nil, err
	}
	return s.hydrateRows(ctx, rows)
}

// MaxRecentPlayHistory 「最近播放」跨上下文查询单次返回条数上限。
const MaxRecentPlayHistory = 200

// ListRecent 跨上下文返回最近播放的歌曲（按歌曲去重、播放时间倒序，含完整歌曲详情）。
// 供「最近播放」页使用：用户不关心当时在哪个上下文听的，只关心「昨天那首」。
func (s *PlayHistoryService) ListRecent(ctx context.Context, limit, offset int) (models.PlayHistoryListResponse, error) {
	if limit <= 0 {
		limit = MaxRecentPlayHistory
	}
	if limit > MaxRecentPlayHistory {
		limit = MaxRecentPlayHistory
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.PlayHistoryRepository().ListRecent(ctx, limit, offset)
	if err != nil {
		return models.PlayHistoryListResponse{}, err
	}
	total, err := s.db.PlayHistoryRepository().CountRecent(ctx)
	if err != nil {
		return models.PlayHistoryListResponse{}, err
	}
	entries, err := s.hydrateRows(ctx, rows)
	if err != nil {
		return models.PlayHistoryListResponse{}, err
	}
	return models.PlayHistoryListResponse{Items: entries, Total: total}, nil
}

// ClearAll 清空全部上下文的播放历史，返回删除条数。
func (s *PlayHistoryService) ClearAll(ctx context.Context) (int, error) {
	return s.db.PlayHistoryRepository().ClearAll(ctx)
}

// hydrateRows 按 rows 顺序把歌曲详情水合进历史条目（List 与 ListRecent 共用）。
// 查不到的 song_id 跳过：外键级联下不该发生，属防御性处理；ListByIDs 失败如实上抛。
func (s *PlayHistoryService) hydrateRows(ctx context.Context, rows []database.PlayHistoryRow) ([]models.PlayHistoryEntry, error) {
	if len(rows) == 0 {
		return []models.PlayHistoryEntry{}, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SongID)
	}
	songs, err := s.db.SongRepository().ListByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*models.Song, len(songs))
	for _, song := range songs {
		byID[song.ID] = song
	}
	entries := make([]models.PlayHistoryEntry, 0, len(rows))
	for _, row := range rows {
		song, ok := byID[row.SongID]
		if !ok {
			continue
		}
		entries = append(entries, models.PlayHistoryEntry{
			Song:      song,
			PlayedAt:  row.PlayedAt,
			PlayCount: row.PlayCount,
		})
	}
	return entries, nil
}

// Clear 清空该上下文的全部播放历史，返回删除条数。
func (s *PlayHistoryService) Clear(ctx context.Context, contextType, contextKey string) (int, error) {
	if !IsValidPlayContextType(contextType) {
		return 0, fmt.Errorf("%w: type %q", ErrInvalidPlayContext, contextType)
	}
	return s.db.PlayHistoryRepository().Clear(ctx, contextType, contextKey)
}

// DeleteEntry 删除单条播放历史，未命中返回 database.ErrNotFound。
func (s *PlayHistoryService) DeleteEntry(ctx context.Context, contextType, contextKey string, songID int64) error {
	if !IsValidPlayContextType(contextType) {
		return fmt.Errorf("%w: type %q", ErrInvalidPlayContext, contextType)
	}
	return s.db.PlayHistoryRepository().DeleteEntry(ctx, contextType, contextKey, songID)
}
