package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"songloft/internal/handlers"
	"songloft/internal/services"
	"songloft/internal/services/managed"
)

// setupManaged 装配托管播放组件（S-03…S-09）：票据签发、当前项准备、
// 听音事件、ingest 队列、BFF 状态机与票据媒体服务。
// kwqq v2 解析凭证经配置下发（缺省时 prepare 失败关闭，不影响其他路径）。
func (a *App) setupManaged() {
	// 装配失败不得阻断主服务启动（managed 域可整体缺席，legacy 路径不受影响）
	defer func() {
		if r := recover(); r != nil {
			slog.Error("managed: 装配 panic（已忽略，托管入口不可用）", "panic", r)
			a.managedHandler = nil
		}
	}()
	dataDir := filepath.Dir(a.config.DBPath)
	workDir := filepath.Join(dataDir, "managed")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		slog.Error("managed: 创建工作目录失败", "error", err)
		return
	}

	sqldb, ok := a.db.(interface{ DB() *sql.DB })
	if !ok {
		slog.Error("managed: 底层 DB 不支持导出 *sql.DB，托管组件未装配")
		return
	}

	issuer := managed.NewTicketIssuer(sqldb.DB())
	store := managed.NewSQLiteStateStore(sqldb.DB())
	resolver := &managed.KwqqClient{
		BaseURL: a.configService.GetString("kwqq_v2_base_url", "http://10.10.10.2:3003"),
		Token:   firstNonEmpty(a.configService.GetString("kwqq_service_token", ""), os.Getenv("KWQQ_SERVICE_TOKEN")),
	}
	downloader := func(ctx context.Context, result *managed.ResolveResult, destDir string) (string, int64, error) {
		return managed.DownloadToFile(ctx, result, destDir, managed.DownloadLimits{
			HostChecker: func(host string) error {
				if services.IsHostnameAllowed(host) {
					return nil
				}
				return fmt.Errorf("host %q refused by private-network policy", host)
			},
		})
	}
	mgr := managed.NewManager(managed.PrepareDeps{
		Resolver:   resolver,
		Downloader: downloader,
		Transcoder: func(ctx context.Context, input, destDir string) (string, error) {
			return managed.TranscodeToMP3(ctx, input, destDir, managed.TranscoderLimits{})
		},
		WorkDir: workDir,
	}).WithStore(store)
	if n, err := mgr.RecoverOnRestart(context.Background()); err != nil {
		slog.Warn("managed: 重启恢复准备操作失败", "error", err)
	} else if n > 0 {
		slog.Info("managed: 重启恢复", "interrupted_ops", n)
	}

	events := managed.NewEventLog(sqldb.DB())
	roots := []string{}
	if a.config.MusicDir != "" {
		roots = append(roots, a.config.MusicDir)
	}
	ingest := managed.NewIngestQueue(sqldb.DB(), managed.IngestLimits{LibraryRoots: roots})
	bff := managed.NewManagedBFF(issuer)
	media := managed.NewMediaServer(issuer, dataDir) // 表示记录已含 managed/ 前缀，Root 是其父目录

	// BFF→bridge 命令通道（B-08）：env 优先，未配置时 BFF 只维护本地状态机。
	h := handlers.NewManagedHandler(bff, mgr, issuer, events, ingest, media)
	h.SetBridgeClient(managed.NewBridgeClient(
		firstNonEmpty(os.Getenv("BRIDGE_BASE_URL"), a.configService.GetString("bridge_base_url", "")),
		firstNonEmpty(os.Getenv("BRIDGE_MUSIC_TOKEN"), a.configService.GetString("bridge_music_token", "")),
		firstNonEmpty(os.Getenv("BRIDGE_TARGET"), a.configService.GetString("bridge_target", "lx06")),
	))
	h.SetIngestScanFunc(func(ctx context.Context, n *managed.IngestNotification) error {
		// 单文件入库：同步扫其所在目录并落库（导入按文件幂等）。
		// 必须走 SongService 的导入路径——只调 Scanner 只收集文件、不写曲库；
		// 且要跳过「10 秒内视为正在写入」窗口（完成通知对应的是已原子发布文件）。
		return a.songService.ScanAndImportIngest(ctx, n.FilePath)
	})
	a.managedHandler = h
	slog.Info("managed: 托管播放组件已装配", "workdir", workDir)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
