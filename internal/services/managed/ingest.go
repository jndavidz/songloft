package managed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"songloft/internal/database/sqlc"
)

// S-08：WebGUI completed outbox → Songloft 增量扫描与失败补偿。
//
// - 只读取已原子发布的文件：临时/半文件/加密件/路径穿越/未发布路径明确拒绝
//   （永久拒绝，可见、可审计），绝不扫临时目录；
// - 同一完成事件重复投递只产生一次逻辑入库（notif_id 幂等）；
// - 乱序无影响：每条通知自带身份与路径，按 id 顺序独立处理；
// - 失败重试有界（MaxAttempts），pending 行持久化——进程中断/长期断连
//   后重启由 ProcessAll 续跑（复用有界状态，不建新常驻服务）；
// - kwqq 不可用不影响本路径：本地文件走保真链（E7），API 播放 URL 永不进入。

// 通知状态。
const (
	IngestPending  = "pending"
	IngestDone     = "done"
	IngestFailed   = "failed"   // 可重试耗尽后保留，供补偿/人工重放
	IngestRejected = "rejected" // 永久拒绝（穿越/临时/加密件），可见可审计
)

// IngestNotification 一条下载完成通知。
type IngestNotification struct {
	NotifID   string    `json:"notif_id"`
	FilePath  string    `json:"file_path"`
	Identity  string    `json:"identity"` // 结构化身份 JSON（source_data 形态），可选
	CreatedAt time.Time `json:"created_at"`
}

// IngestLimits 校验与重试边界。
type IngestLimits struct {
	LibraryRoots []string // 正式曲库根目录（白名单）；空 = 拒绝一切
	MaxAttempts  int      // 默认 3
}

// ScanFunc 对单个已发布文件执行真实入库索引（生产接曲库扫描；测试注入）。
type ScanFunc func(ctx context.Context, notif *IngestNotification) error

// IngestQueue 通知队列：去重、校验、重试与重启补偿。
type IngestQueue struct {
	q      *sqlc.Queries
	limits IngestLimits
}

func NewIngestQueue(db *sql.DB, limits IngestLimits) *IngestQueue {
	if limits.MaxAttempts <= 0 {
		limits.MaxAttempts = 3
	}
	return &IngestQueue{q: sqlc.New(db), limits: limits}
}

// tempEncryptedExts 是永久拒绝的扩展名：临时/半文件与加密件
// （ekey 域：ncm/kgm/kge/mflac/mgg/qmc 系）——绝不进入保真曲库。
var tempEncryptedExts = map[string]bool{
	".tmp": true, ".temp": true, ".part": true, ".partial": true,
	".crdownload": true, ".download": true, "~": true,
	".ncm": true, ".kgm": true, ".kge": true, ".kgtemp": true,
	".mflac": true, ".mflac0": true, ".mflac1": true,
	".mgg": true, ".mgg0": true, ".mgg1": true,
	".qmc0": true, ".qmc2": true, ".qmc3": true, ".qmcflac": true, ".qmcogg": true,
	".mogg": true,
}

var (
	ErrPathOutsideLibrary = &typedError{"managed: ingest path outside library roots"}
	ErrTempOrEncrypted    = &typedError{"managed: ingest path is temp or encrypted content"}
	ErrNotPublished       = &typedError{"managed: ingest file missing or incomplete"}
)

// validate 只接受正式根目录下、已原子发布、非临时/加密的文件。
func (q *IngestQueue) validate(notif *IngestNotification) error {
	if notif.NotifID == "" {
		return &typedError{"managed: ingest notification requires id"}
	}
	clean := filepath.Clean(notif.FilePath)
	if !filepath.IsAbs(clean) {
		return ErrPathOutsideLibrary
	}
	inside := false
	for _, root := range q.limits.LibraryRoots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absRoot, clean)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			inside = true
			break
		}
	}
	if !inside {
		return ErrPathOutsideLibrary
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if tempEncryptedExts[ext] || strings.HasSuffix(clean, "~") {
		return ErrTempOrEncrypted
	}
	st, err := os.Stat(clean)
	if err != nil || st.IsDir() {
		return ErrNotPublished // 未发布/半文件（原子发布前不可见是发布方的契约）
	}
	return nil
}

// Submit 接收一条完成通知：校验失败 → rejected（永久、可见）；重复投递 →
// 幂等无操作；通过 → pending 等待扫描。
func (q *IngestQueue) Submit(ctx context.Context, notif *IngestNotification) (state string, err error) {
	now := time.Now().Unix()
	state, lastErr, rejectErr := IngestPending, "", error(nil)
	if verr := q.validate(notif); verr != nil {
		state, lastErr, rejectErr = IngestRejected, verr.Error(), verr
	}
	_, err = q.q.InsertIngestNotification(ctx, sqlc.InsertIngestNotificationParams{
		NotifID: orDefault(notif.NotifID, emptyID()), FilePath: notif.FilePath,
		Identity: notif.Identity, State: state, LastError: lastErr,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		// ON CONFLICT DO NOTHING：重复投递（ErrNoRows）→ 查既有状态，
		// 不产生第二次逻辑入库
		if errors.Is(err, sql.ErrNoRows) && state == IngestPending {
			if st, gerr := q.q.GetIngestNotificationState(ctx, notif.NotifID); gerr == nil {
				return st, nil
			}
		}
		return "", err
	}
	if state == IngestRejected {
		return state, rejectErr
	}
	return state, nil
}

// ProcessAll 处理全部 pending 通知（启动补偿与周期触发共用同一入口；
// 调用方决定节奏，本包不是常驻服务）。返回 (成功, 失败, 拒绝)。
// 扫描失败 → attempts++；达上限转 failed 保留现场，等待后续补偿重放。
func (q *IngestQueue) ProcessAll(ctx context.Context, scan ScanFunc) (done, failed, rejected int, err error) {
	const batch = 100
	for {
		rows, err := q.q.ListPendingIngestNotifications(ctx, batch)
		if err != nil {
			return done, failed, rejected, err
		}
		if len(rows) == 0 {
			return done, failed, rejected, nil
		}
		for _, r := range rows {
			notif := &IngestNotification{
				NotifID: r.NotifID, FilePath: r.FilePath, Identity: r.Identity,
				CreatedAt: time.Unix(r.CreatedAt, 0),
			}
			scanErr := scan(ctx, notif)
			attempts := r.Attempts + 1
			state := IngestDone
			lastErr := ""
			if scanErr != nil {
				lastErr = scanErr.Error()
				if int(attempts) >= q.limits.MaxAttempts {
					state = IngestFailed
					failed++
				} else {
					state = IngestPending // 保留 pending，下一轮重试
				}
			} else {
				done++
			}
			if uerr := q.q.UpdateIngestNotificationResult(ctx, sqlc.UpdateIngestNotificationResultParams{
				State: state, Attempts: attempts, LastError: lastErr,
				UpdatedAt: time.Now().Unix(), NotifID: r.NotifID,
			}); uerr != nil {
				return done, failed, rejected, uerr
			}
		}
		if len(rows) < batch {
			return done, failed, rejected, nil
		}
	}
}

// CompensateInterrupted 是重启补偿的显式入口：恢复时把因进程中断而停留在
// pending 的行原样保留（无需转换）——ProcessAll 天然续跑；attempts 已计的
// 不重置，防止无限重试。返回 pending 数供诊断。
func (q *IngestQueue) CompensateInterrupted(ctx context.Context) (int, error) {
	rows, err := q.q.ListPendingIngestNotifications(ctx, 1000)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

func emptyID() string { return fmt.Sprintf("anon-%d", time.Now().UnixNano()) }

func lastError(msg string) error { return &typedError{msg: msg} }
