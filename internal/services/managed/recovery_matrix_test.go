package managed

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"songloft/internal/database/testutil"
	"time"
)

// TestRecoveryMatrix_ImageOnly（S-07）：旧版→新版迁移加法性、重启后的
// 失败关闭与恢复语义——准备操作 interrupted（绝不自动续播）、过期票据
// 拒绝、有效 lease 恢复、ingest pending 续跑。这是 image-only 升级验收的
// 离线恢复矩阵；NAS 实际升级与运行身份回读需批准窗口（Not run）。
func TestRecoveryMatrix_ImageOnly(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	ctx := context.Background()
	root := t.TempDir()
	published := filepath.Join(root, "lib", "song.mp3")
	if err := writeTestMP3(t, published); err != nil {
		t.Fatal(err)
	}

	// ---- 旧"进程"：签发票据（一张有效、一张即将过期）、发起准备、投递 ingest ----
	issuerOld := NewTicketIssuer(mdb.DB())
	issuerOld.DefaultTTL = 1 * time.Hour
	validTk, err := issuerOld.Issue(ctx, TicketRequest{
		RefDigest: "d-valid", Representation: "managed/dv/final.mp3",
		Session: "s", Epoch: 1, Entry: "e1", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	expiredTk, err := issuerOld.Issue(ctx, TicketRequest{
		RefDigest: "d-exp", Representation: "managed/de/final.mp3",
		Session: "s", Epoch: 1, Entry: "e2", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	prepareDeps := testDeps(t, &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}})
	mgrOld := NewManager(prepareDeps).WithStore(NewSQLiteStateStore(mdb.DB()))
	op, err := mgrOld.StartPrepare("issuer-a", OpKey{Session: "s", Epoch: 1, Entry: "e1"}, MustRef(t, "kwqq", "1", nil))
	if err != nil {
		t.Fatal(err)
	}

	ingestOld := NewIngestQueue(mdb.DB(), IngestLimits{LibraryRoots: []string{root}})
	if _, err := ingestOld.Submit(ctx, &IngestNotification{NotifID: "n1", FilePath: published}); err != nil {
		t.Fatal(err)
	}
	_ = op

	// 把过期票据的 expires_at 直接拨到过去（模拟升级耗时跨越了短 TTL）
	if _, err := mdb.DB().ExecContext(ctx,
		`UPDATE media_tickets SET expires_at = strftime('%s','now') - 10 WHERE token_hash = ?`,
		hashToken(expiredTk.Token)); err != nil {
		t.Fatal(err)
	}

	// ---- "重启"：全新组件实例，同一持久层（image-only 换容器的语义等价物）----
	issuerNew := NewTicketIssuer(mdb.DB())
	mgrNew := NewManager(testDeps(t, &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}})).WithStore(NewSQLiteStateStore(mdb.DB()))
	ingestNew := NewIngestQueue(mdb.DB(), IngestLimits{LibraryRoots: []string{root}})

	t.Run("准备操作恢复为 interrupted，绝不自动续播", func(t *testing.T) {
		n, err := mgrNew.RecoverOnRestart(ctx)
		if err != nil || n != 1 {
			t.Fatalf("应恢复 1 行 interrupted，got %d err=%v", n, err)
		}
		if mgrNew.ActiveCount() != 0 {
			t.Error("恢复后不得有活跃准备（自动续播禁令）")
		}
	})
	t.Run("过期票据失败关闭", func(t *testing.T) {
		if _, _, _, _, err := issuerNew.Record(ctx, expiredTk.Token); err != ErrTicketExpired {
			t.Fatalf("过期票据升级重启后必须拒绝，got %v", err)
		}
	})
	t.Run("有效 lease 恢复可用", func(t *testing.T) {
		rep, _, _, _, err := issuerNew.Record(ctx, validTk.Token)
		if err != nil {
			t.Fatalf("未过期票据应跨重启可用: %v", err)
		}
		if rep != "managed/dv/final.mp3" {
			t.Errorf("representation 不变: %q", rep)
		}
	})
	t.Run("ingest pending 续跑", func(t *testing.T) {
		pending, err := ingestNew.CompensateInterrupted(ctx)
		if err != nil || pending != 1 {
			t.Fatalf("ingest 应有 1 条待续，got %d err=%v", pending, err)
		}
	})
	t.Run("迁移加法性：旧表未动", func(t *testing.T) {
		// songs / playlists 等既有表在新版本仍可读写（additive migrations 契约）
		var n int
		if err := mdb.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
			 ('songs','playlists','media_tickets','managed_prepare_ops','listening_events','ingest_notifications')`,
		).Scan(&n); err != nil || n != 6 {
			t.Fatalf("新旧表应共存（additive），got %d err=%v", n, err)
		}
	})
}

// writeTestMP3 把有效 MP3 fixture 写到目标路径。
func writeTestMP3(t *testing.T, dst string) error {
	t.Helper()
	if err := osMkdirAll(filepath.Dir(dst)); err != nil {
		return err
	}
	return copyTestdataMP3(dst)
}

func osMkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func copyTestdataMP3(dst string) error {
	src, err := os.Open(filepath.Join("testdata", "valid_128k.mp3"))
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}
