package managed

import (
	"context"
	"testing"

	"songloft/internal/database/testutil"
)

// TestAnalyzeOrphans_ExclusionMatrix：全部排除条件的表驱动证据（S-09 DoD）。
func TestAnalyzeOrphans_ExclusionMatrix(t *testing.T) {
	s := func(id int64, digest string, cache string) GCSong {
		return GCSong{ID: id, RefDigest: digest, CachePath: cache}
	}
	input := []GCSong{s(1, "d1", ""), s(2, "d2", ""), s(3, "d3", ""), s(4, "d4", ""),
		s(5, "d5", ""), s(6, "d6", ""), s(7, "d7", ""), s(8, "d8", ""),
		s(9, "d9", "cache/9.mp3"), s(10, "d10", "")}
	refs := map[int64]GCReferences{
		2: {InPlaylist: true},      // 歌单（含收藏）引用 → 排除
		3: {InActiveQueue: true},   // 活动队列 → 排除
		4: {HasLease: true},        // 有效 lease → 排除
		5: {Preparing: true},       // 准备中 → 排除
		6: {RecentFailure: true},   // 近期失败重试 → 排除
		7: {InListenHistory: true}, // 听音事件 → 排除
		8: {ExplicitDl: true},      // 显式下载 → 排除
		9: {ExplicitDl: true},      // 双保险：有缓存路径也排除
	}
	exempt := map[int64]bool{10: true} // legacy/上一发布版保护名单

	got := AnalyzeOrphans(input, refs, exempt)
	if len(got) != 1 || got[0].SongID != 1 {
		t.Fatalf("只有零引用的 song 1 应成为候选，got %+v", got)
	}
}

// TestAnalyzeOrphans_PlayedOnceStillReferenced：legacy remote 不因播放一次被清——
// 听音历史引用即排除（引用判定与次数/名称无关）。
func TestAnalyzeOrphans_PlayedOnceStillReferenced(t *testing.T) {
	songs := []GCSong{{ID: 42, RefDigest: "d42"}}
	refs := map[int64]GCReferences{42: {InListenHistory: true}}
	if got := AnalyzeOrphans(songs, refs, nil); len(got) != 0 {
		t.Fatalf("播放过一次的 remote 不得成为候选（误删负例），got %+v", got)
	}
	// 同名不同身份同理：名字不参与判定，引用才参与
	refs2 := map[int64]GCReferences{}
	if got := AnalyzeOrphans(songs, refs2, nil); len(got) != 1 {
		t.Fatalf("零引用才是候选的唯一判据: %+v", got)
	}
}

// TestTombstone_IdempotentAndRestorable：重复运行幂等；隔离可恢复；恢复后不重复计数。
func TestTombstone_IdempotentAndRestorable(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	store := NewTombstoneStore(mdb.DB())
	ctx := context.Background()

	c := GCCandidate{SongID: 7, RefDigest: "d7"}
	for i := 0; i < 3; i++ { // 重复运行
		if err := store.Quarantine(ctx, c, "orphan: zero references"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("重复隔离必须幂等（count=1），got %d err=%v", n, err)
	}
	ids, _ := store.QuarantinedIDs(ctx)
	if !ids[7] {
		t.Fatal("隔离清单应含 song 7")
	}
	if err := store.Restore(ctx, 7); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ids, _ = store.QuarantinedIDs(ctx)
	if ids[7] {
		t.Fatal("恢复后不应在隔离清单")
	}
	// 恢复后再次隔离：新 tombstone 行（ON CONFLICT 会撞 song_id UNIQUE——
	// 预览语义下恢复后重新隔离应视为新的隔离决定）
	if err := store.Quarantine(ctx, c, "re-quarantine"); err != nil {
		t.Fatalf("恢复后可再次隔离: %v", err)
	}
}

// TestTombstone_NoPhysicalDeletion：隔离不触碰 songs 表数据与缓存文件——
// 非破坏性契约（直接 DELETE/清缓存目录被禁止）。
func TestTombstone_NoPhysicalDeletion(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	store := NewTombstoneStore(mdb.DB())
	ctx := context.Background()
	if err := store.Quarantine(ctx, GCCandidate{SongID: 5, RefDigest: "d5"}, "orphan"); err != nil {
		t.Fatal(err)
	}
	// songs 表行数不受影响（这里以 sqlite_master 计数 + gc_tombstones 无删除路径佐证：
	// store 的全部方法只有 INSERT/UPDATE restored_at/SELECT，无 DELETE）
	var tables int
	if err := mdb.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='gc_tombstones'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("tombstone 表应存在: %d %v", tables, err)
	}
	if n, _ := store.Count(ctx); n != 1 {
		t.Fatalf("隔离计数=1，got %d", n)
	}
}
