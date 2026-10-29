package managed

import (
	"context"
	"testing"
	"time"

	"songloft/internal/database/testutil"
)

// TestRefFromSourceData：kwqq source_data → TrackRef 的身份映射契约。
func TestRefFromSourceData(t *testing.T) {
	t.Run("migu c → copyrightId 保留", func(t *testing.T) {
		ref, err := RefFromSourceData(`{"s":"migu","i":"m01","c":"6900001"}`, nil)
		if err != nil {
			t.Fatalf("migu: %v", err)
		}
		if ref.Extra["copyrightId"] != "6900001" {
			t.Errorf("copyrightId 应保留，got %v", ref.Extra)
		}
	})
	t.Run("kwqq c 非身份字段被契约丢弃", func(t *testing.T) {
		ref, err := RefFromSourceData(`{"s":"kwqq","i":"12345","c":"x"}`, nil)
		if err != nil {
			t.Fatalf("kwqq: %v", err)
		}
		if _, ok := ref.Extra["copyrightId"]; ok {
			t.Error("kwqq 源不得携带 copyrightId（契约白名单外）")
		}
	})
	t.Run("bilibili 缺 cid/page 明确失败", func(t *testing.T) {
		if _, err := RefFromSourceData(`{"s":"bilibili","i":"BV1x"}`, nil); err == nil {
			t.Fatal("bilibili 缺分 P 身份必须显式失败，不静默错配")
		}
	})
	t.Run("WithExtra 选中时补全 cid（digest 随身份更新）", func(t *testing.T) {
		base, err := MakeRef("bilibili", "BV1x", map[string]any{"page": 1}, nil)
		if err != nil {
			t.Fatalf("base: %v", err)
		}
		completed, err := base.WithExtra(map[string]any{"cid": "30251"})
		if err != nil {
			t.Fatalf("补全 cid 后应通过校验: %v", err)
		}
		d1, _ := base.Digest()
		d2, _ := completed.Digest()
		if d1 == d2 {
			t.Error("补全 cid 后 digest 必须变化（同一 BV 不同分 P 不串）")
		}
		if completed.Source != "bilibili" {
			t.Errorf("WithExtra 不得变更 source，got %s", completed.Source)
		}
	})
	t.Run("畸形 source_data 失败", func(t *testing.T) {
		if _, err := RefFromSourceData(`not-json`, nil); err == nil {
			t.Fatal("非 JSON source_data 应失败")
		}
	})
}

func MustRef(t *testing.T, source, id string, extra map[string]any) *Ref {
	t.Helper()
	ref, err := MakeRef(source, id, extra, nil)
	if err != nil {
		t.Fatalf("MakeRef(%s,%s): %v", source, id, err)
	}
	return ref
}

// TestManager_SQLiteStorePersistence：同步 Insert + 异步 Update + 重启恢复。
func TestManager_SQLiteStorePersistence(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	store := NewSQLiteStateStore(mdb.DB())

	gate := make(chan struct{})
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		<-gate
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res)).WithStore(store)

	key := OpKey{Session: "sess-r", Epoch: 7, Entry: "e1"}
	op, err := m.StartPrepare("issuer-x", key, MustRef(t, "kwqq", "1", nil))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// 同步 Insert：启动后立即能在持久层看到 preparing 行
	rows, err := store.ListActive(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("启动后持久层应有 1 条 preparing，got %d err=%v", len(rows), err)
	}
	if rows[0].Key != key || rows[0].Issuer != "issuer-x" {
		t.Errorf("行内容不符: %+v", rows[0])
	}

	// 完成后行转为终态（异步 Update，轮询等待）
	close(gate)
	select {
	case <-op.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ = store.ListActive(context.Background())
		if len(rows) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 0 {
		t.Errorf("完成后持久层不应再有 preparing 行，got %d", len(rows))
	}

	// 模拟重启：手工插入 preparing 行，RecoverOnRestart 转 interrupted
	ctx := context.Background()
	stale := &PrepareOp{
		Key: OpKey{Session: "sess-r", Epoch: 8, Entry: "e2"}, Issuer: "issuer-x",
		RefDigest: "d", State: StatePreparing,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store.Insert(ctx, stale); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	n, err := m.RecoverOnRestart(ctx)
	if err != nil || n != 1 {
		t.Fatalf("恢复应转 1 行 interrupted，got %d err=%v", n, err)
	}
	rows, _ = store.ListActive(ctx)
	if len(rows) != 0 {
		t.Errorf("恢复后不应残留 preparing，got %d", len(rows))
	}
}

// TestManager_InsertFailureRollsBackRegistration：持久化失败时操作不启动。
func TestManager_InsertFailureRollsBackRegistration(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	store := NewSQLiteStateStore(mdb.DB())
	m := NewManager(PrepareDeps{WorkDir: t.TempDir()}).WithStore(store)

	// 关闭底层 DB 使 Insert 失败
	if err := mdb.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, err := m.StartPrepare("i", OpKey{Session: "s", Epoch: 1, Entry: "e"}, MustRef(t, "kwqq", "1", nil)); err == nil {
		t.Fatal("Insert 失败应拒绝启动")
	}
	if m.ActiveCount() != 0 {
		t.Errorf("失败启动不得残留活跃操作，got %d", m.ActiveCount())
	}
}
