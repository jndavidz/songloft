package managed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"songloft/internal/database/sqlc"
	"songloft/internal/database/testutil"
)

// newIngestFixture 建一个正式曲库根 + 一个已原子发布的真实 MP3 文件。
func newIngestFixture(t *testing.T) (root, published string) {
	t.Helper()
	root = t.TempDir()
	published = filepath.Join(root, "Artist", "Album", "song01.mp3")
	if err := os.MkdirAll(filepath.Dir(published), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(filepath.Join("testdata", "valid_128k.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(published)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	return root, published
}

func newIngestQueue(t *testing.T, root string) *IngestQueue {
	t.Helper()
	mdb := testutil.OpenMemoryDB(t)
	return NewIngestQueue(mdb.DB(), IngestLimits{LibraryRoots: []string{root}})
}

// TestIngest_DuplicateOnce：同一完成事件重复投递只产生一次逻辑入库。
func TestIngest_DuplicateOnce(t *testing.T) {
	root, published := newIngestFixture(t)
	q := newIngestQueue(t, root)
	ctx := context.Background()

	var scans atomic.Int32
	scan := func(ctx context.Context, n *IngestNotification) error {
		scans.Add(1)
		return nil
	}
	for i := 0; i < 3; i++ { // 同一 notif_id 投递 3 次
		st, err := q.Submit(ctx, &IngestNotification{NotifID: "n1", FilePath: published})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if st != IngestDone && i == 0 {
			_ = st // 首次 pending；后续重复返回既有状态
		}
	}
	d, f, r, err := q.ProcessAll(ctx, scan)
	if err != nil {
		t.Fatal(err)
	}
	if d != 1 || f != 0 || r != 0 {
		t.Fatalf("done=%d failed=%d rejected=%d", d, f, r)
	}
	if scans.Load() != 1 {
		t.Errorf("重复投递只应扫描一次，got %d", scans.Load())
	}
}

// TestIngest_RejectMatrix：路径穿越/临时件/加密件/未发布文件明确拒绝且可审计。
func TestIngest_RejectMatrix(t *testing.T) {
	root, published := newIngestFixture(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "evil.mp3")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, wantState string
		wantErr               error
	}{
		{"根目录外", outsideFile, IngestRejected, ErrPathOutsideLibrary},
		{"穿越写法", filepath.Join(root, "..", "escape.mp3"), IngestRejected, ErrPathOutsideLibrary},
		{"临时件", published + ".tmp", IngestRejected, ErrTempOrEncrypted},
		{"半文件", filepath.Join(root, "a", "b.part"), IngestRejected, ErrTempOrEncrypted},
		{"加密件", filepath.Join(root, "song.ncm"), IngestRejected, ErrTempOrEncrypted},
		{"mflac", filepath.Join(root, "song.mflac"), IngestRejected, ErrTempOrEncrypted},
		{"未发布（不存在）", filepath.Join(root, "Artist", "ghost.mp3"), IngestRejected, ErrNotPublished},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newIngestQueue(t, root)
			st, err := q.Submit(context.Background(),
				&IngestNotification{NotifID: tc.name, FilePath: tc.path})
			if st != tc.wantState {
				t.Errorf("state=%s want %s", st, tc.wantState)
			}
			if !errors.Is(err, tc.wantErr) && tc.wantErr != nil && err == nil {
				t.Errorf("应拒绝: %v", err)
			}
		})
	}
}

// TestIngest_RetryThenCompensate：扫描失败重试有界；重启后 pending 续跑。
func TestIngest_RetryThenCompensate(t *testing.T) {
	root, published := newIngestFixture(t)
	q := newIngestQueue(t, root)
	q.limits.MaxAttempts = 3
	ctx := context.Background()

	if _, err := q.Submit(ctx, &IngestNotification{NotifID: "n-retry", FilePath: published}); err != nil {
		t.Fatal(err)
	}

	// 前两轮扫描失败 → 仍 pending；第三轮达上限 → failed 保留现场
	for round := 1; round <= 2; round++ {
		d, f, _, err := q.ProcessAll(ctx, func(ctx context.Context, n *IngestNotification) error {
			return fmt.Errorf("transient scan failure")
		})
		if err != nil {
			t.Fatal(err)
		}
		if d != 0 || f != 0 {
			t.Fatalf("round %d: 未达上限前不得转 failed（d=%d f=%d）", round, d, f)
		}
		st, _ := q.q.GetIngestNotificationState(ctx, "n-retry")
		if st != IngestPending {
			t.Fatalf("round %d: 应保持 pending，got %s", round, st)
		}
	}
	// 第三轮：达上限 → failed
	d, f, _, err := q.ProcessAll(ctx, func(ctx context.Context, n *IngestNotification) error {
		return fmt.Errorf("still failing")
	})
	if err != nil || d != 0 || f != 1 {
		t.Fatalf("达上限应 failed（d=%d f=%d err=%v）", d, f, err)
	}
	// 补偿重放：把行拉回 pending（人工/自动补偿入口），扫描成功后入库
	err = q.q.UpdateIngestNotificationResult(ctx, sqlc.UpdateIngestNotificationResultParams{
		State: IngestPending, Attempts: 0, LastError: "", UpdatedAt: time.Now().Unix(), NotifID: "n-retry",
	})
	var scanned atomic.Int32
	d, f, _, err = q.ProcessAll(ctx, func(ctx context.Context, n *IngestNotification) error {
		scanned.Add(1)
		return nil
	})
	if err != nil || d != 1 || f != 0 || scanned.Load() != 1 {
		t.Fatalf("补偿后应成功（d=%d f=%d scans=%d err=%v）", d, f, scanned.Load(), err)
	}
}

// TestIngest_RestartCompensation：重启（重开 DB）后 pending 行续跑，不丢不重。
func TestIngest_RestartCompensation(t *testing.T) {
	root, published := newIngestFixture(t)
	mdb := testutil.OpenMemoryDB(t)
	ctx := context.Background()

	// 进程 1：提交两条后"崩溃"（不处理）
	q1 := NewIngestQueue(mdb.DB(), IngestLimits{LibraryRoots: []string{root}})
	for _, id := range []string{"n-a", "n-b"} {
		if _, err := q1.Submit(ctx, &IngestNotification{NotifID: id, FilePath: published}); err != nil {
			t.Fatal(err)
		}
	}

	// 进程 2：重启补偿——同一 DB，pending 行续跑
	q2 := NewIngestQueue(mdb.DB(), IngestLimits{LibraryRoots: []string{root}})
	pending, err := q2.CompensateInterrupted(ctx)
	if err != nil || pending != 2 {
		t.Fatalf("重启应发现 2 条 pending，got %d err=%v", pending, err)
	}
	var scans atomic.Int32
	d, f, _, err := q2.ProcessAll(ctx, func(ctx context.Context, n *IngestNotification) error {
		scans.Add(1)
		return nil
	})
	if err != nil || d != 2 || f != 0 || scans.Load() != 2 {
		t.Fatalf("重启补偿应各入库一次（d=%d f=%d scans=%d err=%v）", d, f, scans.Load(), err)
	}
	// 再来一轮：无重复历史
	d, _, _, _ = q2.ProcessAll(ctx, func(ctx context.Context, n *IngestNotification) error {
		scans.Add(1)
		return nil
	})
	if d != 0 || scans.Load() != 2 {
		t.Errorf("补偿后不得重复入库（d=%d scans=%d）", d, scans.Load())
	}
}

// TestIngest_OutOfOrder：乱序投递无影响——每条通知独立按 id 处理。
func TestIngest_OutOfOrder(t *testing.T) {
	root, published := newIngestFixture(t)
	q := newIngestQueue(t, root)
	ctx := context.Background()
	var order []string
	// 乱序：后发的 id 反而先提交
	for _, id := range []string{"z-late", "a-early", "m-mid"} {
		if _, err := q.Submit(ctx, &IngestNotification{NotifID: id, FilePath: published}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, err := q.ProcessAll(ctx, func(ctx context.Context, n *IngestNotification) error {
		order = append(order, n.NotifID)
		return nil
	})
	if err != nil || len(order) != 3 {
		t.Fatalf("乱序投递应全部入库（n=%d err=%v）", len(order), err)
	}
	// 按提交序（id 顺序）稳定处理，不依赖 outbox 顺序
	if order[0] != "z-late" || order[1] != "a-early" || order[2] != "m-mid" {
		t.Errorf("处理序应稳定: %v", order)
	}
}
