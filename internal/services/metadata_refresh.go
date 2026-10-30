package services

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"songloft/internal/database/sqlc"
	"songloft/internal/models"
	"songloft/internal/services/resourcebudget"
)

// dropFilenameFallbackTitle 拦截 Extract 对无标题标签文件的「用文件名当 title」回退结果。
// 缓存回填时 filePath 是缓存文件（名为 `{songID}.{plugin_entry_path}_{dedup_key}`），
// 该文件名绝不是真实歌名；若 title 恰好等于其去扩展名 base，说明是回退产物，返回 ""，
// 避免把缓存文件名写进 songs.title（songloft-org/songloft#286）。真实 tag 标题不受影响。
func dropFilenameFallbackTitle(title, filePath string) string {
	if title == strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath)) {
		return ""
	}
	return title
}

// NeedsMetadata 判断歌曲是否缺少技术元数据（与 ListSongsNeedingMetadata SQL 条件一致）。
func NeedsMetadata(song *models.Song) bool {
	return song.Type == models.TypeRemote &&
		(song.Duration == 0 || song.BitRate == 0 || song.SampleRate == 0 || song.Format == "")
}

// RefreshSongsBackground 对刚导入、缺失技术元数据的网络歌曲发起后台异步探测补齐。
//
// 限并发（避免整目录导入时打爆服务端与上游音源），每首独立超时；RefreshSong 自带 inflight 去重。
// waitForIdle 非空时，每首探测前调用它退避以让路活跃下载（issue #265）；为 nil 则不退避（如
// jsplugin songs.create 桥接路径暂无下载闸门，inflight 去重 + 并发上限已足够收敛占用）。
//
// 供 HTTP 导入路径（handlers.SongHandler.probeRemoteSongsMetadata，带下载让路）与 jsplugin
// songs.create 桥接路径共用：wendav 等 WebDAV 音源无法自带时长，导入后若不探测，duration 会
// 长期为 0，MIot 插件无法据此注册切歌定时器，表现为「单曲循环、不推进列表」
// （songloft-org/songloft#437）。
func (d *MetadataRefresher) RefreshSongsBackground(songs []*models.Song, waitForIdle func()) {
	if d == nil {
		return
	}
	pending := make([]*models.Song, 0, len(songs))
	for _, song := range songs {
		if NeedsMetadata(song) {
			// 复制一份，避免后台 goroutine 与调用方共享指针
			copied := *song
			pending = append(pending, &copied)
		}
	}
	if len(pending) == 0 {
		return
	}

	go func() {
		// issue #265：探测走 ffprobe + ytdlp 插件唯一 worker，与批量下载撞车会打满 CPU 并把
		// 下载解析挤到 30s 超时判死。故 (1) 从源头收敛并发——改由进程资源预算的 probe 分区限
		// （容量 2 + Reserve：收编前是「每次调用各开一套 cap=2」，两批导入并发就是 4 路 ffprobe）；
		// (2) 每首探测前若有活跃下载则退避让路，把 worker 让给下载解析。
		// 探测是尽力而为的后台补齐，让路/延后无副作用。
		var wg sync.WaitGroup
		for _, song := range pending {
			if waitForIdle != nil {
				waitForIdle()
			}
			releaseProbe, err := resourcebudget.ProcessBudget().Acquire(context.Background(), resourcebudget.PartitionProbe)
			if err != nil {
				slog.Warn("元数据探测被资源预算拒绝，剩余项留待下次补齐", "error", err)
				break
			}
			wg.Add(1)
			go func(s *models.Song) {
				defer wg.Done()
				defer releaseProbe()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				d.RefreshSong(ctx, s, "", nil)
			}(song)
		}
		wg.Wait()
		slog.Info("导入歌曲元数据探测完成", "count", len(pending))
	}()
}

type MetadataRefreshStatus = string

const (
	MetadataRefreshIdle       MetadataRefreshStatus = "idle"
	MetadataRefreshRunning    MetadataRefreshStatus = "running"
	MetadataRefreshCancelling MetadataRefreshStatus = "cancelling"
	MetadataRefreshDone       MetadataRefreshStatus = "done"
	MetadataRefreshCancelled  MetadataRefreshStatus = "cancelled"
	MetadataRefreshFailed     MetadataRefreshStatus = "failed"
)

type MetadataRefreshProgress struct {
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Processed int    `json:"processed"`
	Failed    int    `json:"failed"`
}

type MetadataRefresher struct {
	mu       sync.Mutex
	progress MetadataRefreshProgress
	cancelFn context.CancelFunc

	listSongs         func(ctx context.Context) ([]sqlc.ListSongsNeedingMetadataRow, error)
	updateMeta        func(ctx context.Context, params sqlc.UpdateSongMetadataParams) error
	updateTags        func(ctx context.Context, params sqlc.UpdateSongTagFieldsParams) error
	resolveURL        func(ctx context.Context, song *models.Song) (string, map[string]string, error)
	extractor         *MetadataExtractor
	remoteTitleSource atomic.Value // stores func() string; "tag": 用标签覆盖 title; "filename"(默认): 不覆盖

	refreshInflight sync.Map // songID -> struct{}, 防止同一首歌并发提取
}

func NewMetadataRefresher(
	listSongs func(ctx context.Context) ([]sqlc.ListSongsNeedingMetadataRow, error),
	updateMeta func(ctx context.Context, params sqlc.UpdateSongMetadataParams) error,
	updateTags func(ctx context.Context, params sqlc.UpdateSongTagFieldsParams) error,
	resolveURL func(ctx context.Context, song *models.Song) (string, map[string]string, error),
	extractor *MetadataExtractor,
) *MetadataRefresher {
	return &MetadataRefresher{
		progress:   MetadataRefreshProgress{Status: MetadataRefreshIdle},
		listSongs:  listSongs,
		updateMeta: updateMeta,
		updateTags: updateTags,
		resolveURL: resolveURL,
		extractor:  extractor,
	}
}

// SetRemoteTitleSource 注入远程歌曲标题来源配置回调。
func (d *MetadataRefresher) SetRemoteTitleSource(fn func() string) {
	d.remoteTitleSource.Store(fn)
}

// shouldOverrideTitle 返回是否应用 tag 标题覆盖。
func (d *MetadataRefresher) shouldOverrideTitle() bool {
	v := d.remoteTitleSource.Load()
	if v == nil {
		return false
	}
	return v.(func() string)() == "tag"
}

func (d *MetadataRefresher) Start() error {
	d.mu.Lock()
	if d.progress.Status == MetadataRefreshRunning || d.progress.Status == MetadataRefreshCancelling {
		d.mu.Unlock()
		return ErrMetadataRefreshRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancelFn = cancel
	d.progress = MetadataRefreshProgress{Status: MetadataRefreshRunning}
	d.mu.Unlock()

	go d.run(ctx)
	return nil
}

func (d *MetadataRefresher) GetProgress() MetadataRefreshProgress {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.progress
}

func (d *MetadataRefresher) Cancel() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancelFn != nil && d.progress.Status == MetadataRefreshRunning {
		d.cancelFn()
		d.progress.Status = MetadataRefreshCancelling
	}
}

func (d *MetadataRefresher) run(ctx context.Context) {
	defer func() {
		d.mu.Lock()
		switch d.progress.Status {
		case MetadataRefreshRunning:
			d.progress.Status = MetadataRefreshDone
		case MetadataRefreshCancelling:
			d.progress.Status = MetadataRefreshCancelled
		}
		d.cancelFn = nil
		d.mu.Unlock()
	}()

	songs, err := d.listSongs(ctx)
	if err != nil {
		slog.Warn("metadata refresh: list songs failed", "error", err)
		d.mu.Lock()
		d.progress.Status = MetadataRefreshFailed
		d.mu.Unlock()
		return
	}

	d.mu.Lock()
	d.progress.Total = len(songs)
	d.mu.Unlock()

	if len(songs) == 0 {
		return
	}

	for _, row := range songs {
		if ctx.Err() != nil {
			break
		}
		d.processOne(ctx, row)
	}
}

func (d *MetadataRefresher) processOne(ctx context.Context, row sqlc.ListSongsNeedingMetadataRow) {
	extractCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 只处理本地有文件的歌曲：本地歌曲用 file_path，网络歌曲用已落地的 cache_path。
	// 未缓存的网络歌曲不参与批量刷新（播放缓存落盘后由 RefreshSongFromFile 自动回填），
	// 避免批量触发插件 URL 解析（可能整批挂死在上游超时上）。
	filePath := row.FilePath
	if row.Type == models.TypeRemote {
		filePath = row.CachePath
	}

	metadata, err := d.extractor.Extract(extractCtx, filePath)
	if err != nil {
		slog.Warn("metadata refresh: extract failed", "songId", row.ID, "filePath", filePath, "error", err)
		d.incFailed()
		return
	}

	title := metadata.Title
	if row.Type == models.TypeRemote {
		// 缓存文件名是 `{songID}.{plugin_entry_path}_{dedup_key}`，不是真实歌名
		title = dropFilenameFallbackTitle(title, filePath)
	}

	coverPath := ""
	if metadata.HasCover && row.CoverPath == "" && row.CoverUrl == "" {
		if cp, err := d.extractor.SaveCover(row.ID, metadata); err == nil {
			coverPath = cp
		} else {
			slog.Debug("metadata refresh: save cover failed", "songId", row.ID, "error", err)
		}
	}
	// 视频无内嵌封面时从视频抽一帧兜底（需 ffmpeg；失败静默降级）。
	if coverPath == "" && metadata.IsVideo && row.CoverPath == "" && row.CoverUrl == "" {
		if cp, err := d.extractor.ExtractCoverFromVideoFile(extractCtx, filePath, metadata.Duration); err == nil && cp != "" {
			coverPath = cp
		} else if err != nil {
			slog.Debug("metadata refresh: extract video frame failed", "songId", row.ID, "error", err)
		}
	}

	if err := d.updateMeta(ctx, sqlc.UpdateSongMetadataParams{
		Column1:    metadata.Duration,
		Duration:   metadata.Duration,
		Column3:    int64(metadata.BitRate),
		BitRate:    int64(metadata.BitRate),
		Column5:    int64(metadata.SampleRate),
		SampleRate: int64(metadata.SampleRate),
		Column7:    metadata.Format,
		Format:     metadata.Format,
		Column9:    title,
		Title:      title,
		Column11:   metadata.Artist,
		Artist:     metadata.Artist,
		Column13:   metadata.Album,
		Album:      metadata.Album,
		Column15:   coverPath,
		CoverPath:  coverPath,
		ID:         row.ID,
	}); err != nil {
		slog.Warn("metadata refresh: update failed", "songId", row.ID, "error", err)
		d.incFailed()
		return
	}

	// tag 覆盖只针对网络歌曲；本地歌曲扫描时已提取过标签，这里仅填空缺（updateMeta 已完成）
	if row.Type == models.TypeRemote && d.updateTags != nil &&
		(metadata.Artist != "" || metadata.Album != "" || (d.shouldOverrideTitle() && title != "")) {
		tagTitle := ""
		if d.shouldOverrideTitle() {
			tagTitle = title
		}
		if err := d.updateTags(ctx, sqlc.UpdateSongTagFieldsParams{
			Column1: tagTitle,
			Title:   tagTitle,
			Column3: metadata.Artist,
			Artist:  metadata.Artist,
			Column5: metadata.Album,
			Album:   metadata.Album,
			ID:      row.ID,
		}); err != nil {
			slog.Warn("metadata refresh: update tag fields failed", "songId", row.ID, "error", err)
		}
	}

	d.mu.Lock()
	d.progress.Processed++
	d.mu.Unlock()
}

func (d *MetadataRefresher) incFailed() {
	d.mu.Lock()
	d.progress.Failed++
	d.mu.Unlock()
}

var ErrMetadataRefreshRunning = fmt.Errorf("metadata refresh is already running")

// RefreshSong 对单首歌曲提取远程元数据并更新数据库。
// resolvedURL 非空时直接使用，否则内部解析。headers 为请求头（可为 nil）。
// 内置 inflight 去重，同一 songID 不会并发执行。
func (d *MetadataRefresher) RefreshSong(ctx context.Context, song *models.Song, resolvedURL string, headers map[string]string) {
	if _, loaded := d.refreshInflight.LoadOrStore(song.ID, struct{}{}); loaded {
		return
	}
	defer d.refreshInflight.Delete(song.ID)

	url := resolvedURL
	if url == "" {
		var err error
		url, headers, err = d.resolveURL(ctx, song)
		if err != nil {
			slog.Warn("auto refresh: resolve url failed", "songID", song.ID, "error", err)
			return
		}
	}

	probe, err := d.extractor.ProbeMetadataFromURL(ctx, url, headers)
	if err != nil {
		slog.Warn("auto refresh: probe failed", "songID", song.ID, "error", err)
		return
	}

	coverPath := probe.CoverPath
	if coverPath == "" && song.CoverPath == "" && song.CoverURL == "" {
		if cp, err := d.extractor.ExtractCoverFromURL(ctx, url, headers); err == nil {
			coverPath = cp
		}
	}

	if err := d.updateMeta(ctx, sqlc.UpdateSongMetadataParams{
		Column1:    probe.Duration,
		Duration:   probe.Duration,
		Column3:    int64(probe.BitRate),
		BitRate:    int64(probe.BitRate),
		Column5:    int64(probe.SampleRate),
		SampleRate: int64(probe.SampleRate),
		Column7:    probe.Format,
		Format:     probe.Format,
		Column9:    probe.Title,
		Title:      probe.Title,
		Column11:   probe.Artist,
		Artist:     probe.Artist,
		Column13:   probe.Album,
		Album:      probe.Album,
		Column15:   coverPath,
		CoverPath:  coverPath,
		ID:         song.ID,
	}); err != nil {
		slog.Warn("auto refresh: update metadata failed", "songID", song.ID, "error", err)
	}

	if d.updateTags != nil && (probe.Artist != "" || probe.Album != "" || (d.shouldOverrideTitle() && probe.Title != "")) {
		tagTitle := ""
		if d.shouldOverrideTitle() {
			tagTitle = probe.Title
		}
		if err := d.updateTags(ctx, sqlc.UpdateSongTagFieldsParams{
			Column1: tagTitle,
			Title:   tagTitle,
			Column3: probe.Artist,
			Artist:  probe.Artist,
			Column5: probe.Album,
			Album:   probe.Album,
			ID:      song.ID,
		}); err != nil {
			slog.Warn("auto refresh: update tag fields failed", "songID", song.ID, "error", err)
		}
	}

	slog.Info("auto refresh: metadata updated", "songID", song.ID,
		"title", probe.Title, "artist", probe.Artist, "duration", probe.Duration)
}

// RefreshSongFromFile 从本地文件提取元数据并更新数据库。
// 作为 FinalizeCache 的兜底路径，当 ProbeMetadataFromURL 失败时使用。
func (d *MetadataRefresher) RefreshSongFromFile(ctx context.Context, song *models.Song, filePath string) {
	if _, loaded := d.refreshInflight.LoadOrStore(song.ID, struct{}{}); loaded {
		return
	}
	defer d.refreshInflight.Delete(song.ID)

	metadata, err := d.extractor.Extract(ctx, filePath)
	if err != nil {
		slog.Warn("cache backfill: extract failed", "songID", song.ID, "error", err)
		return
	}

	// 缓存文件名是 `{songID}.{plugin_entry_path}_{dedup_key}`，不是真实歌名。
	// 远程音频流通常无内嵌标题标签，Extract 会回退用文件名当 title——若不拦下，
	// 会经 updateMeta/updateTags 把缓存文件名写进 songs.title（songloft-org/songloft#286）。
	// 此处识别出「title 恰好等于缓存文件名 base」的回退结果并清空，只保留技术元数据
	// 与真实 tag 的 artist/album/cover 回填；本地扫描与 URL 探测路径不受影响。
	metadata.Title = dropFilenameFallbackTitle(metadata.Title, filePath)

	coverPath := ""
	if metadata.HasCover && song.CoverPath == "" && song.CoverURL == "" {
		if cp, err := d.extractor.SaveCover(song.ID, metadata); err == nil {
			coverPath = cp
		}
	}
	// 视频无内嵌封面时从视频抽一帧兜底（需 ffmpeg；失败静默降级）。
	if coverPath == "" && metadata.IsVideo && song.CoverPath == "" && song.CoverURL == "" {
		if cp, err := d.extractor.ExtractCoverFromVideoFile(ctx, filePath, metadata.Duration); err == nil && cp != "" {
			coverPath = cp
		} else if err != nil {
			slog.Debug("cache backfill: extract video frame failed", "songID", song.ID, "error", err)
		}
	}

	if err := d.updateMeta(ctx, sqlc.UpdateSongMetadataParams{
		Column1:    metadata.Duration,
		Duration:   metadata.Duration,
		Column3:    int64(metadata.BitRate),
		BitRate:    int64(metadata.BitRate),
		Column5:    int64(metadata.SampleRate),
		SampleRate: int64(metadata.SampleRate),
		Column7:    metadata.Format,
		Format:     metadata.Format,
		Column9:    metadata.Title,
		Title:      metadata.Title,
		Column11:   metadata.Artist,
		Artist:     metadata.Artist,
		Column13:   metadata.Album,
		Album:      metadata.Album,
		Column15:   coverPath,
		CoverPath:  coverPath,
		ID:         song.ID,
	}); err != nil {
		slog.Warn("cache backfill: update metadata failed", "songID", song.ID, "error", err)
	}

	if d.updateTags != nil && (metadata.Artist != "" || metadata.Album != "" || (d.shouldOverrideTitle() && metadata.Title != "")) {
		tagTitle := ""
		if d.shouldOverrideTitle() {
			tagTitle = metadata.Title
		}
		if err := d.updateTags(ctx, sqlc.UpdateSongTagFieldsParams{
			Column1: tagTitle,
			Title:   tagTitle,
			Column3: metadata.Artist,
			Artist:  metadata.Artist,
			Column5: metadata.Album,
			Album:   metadata.Album,
			ID:      song.ID,
		}); err != nil {
			slog.Warn("cache backfill: update tag fields failed", "songID", song.ID, "error", err)
		}
	}

	slog.Info("cache backfill: metadata updated", "songID", song.ID)
}
