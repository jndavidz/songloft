package playactivity

import (
	"context"
	"sync"
	"testing"
	"time"

	"songloft/internal/services/requestidentity"
)

func TestSessionFromContext(t *testing.T) {
	t.Run("ctx 无身份 → 一次性独立桶，不共享", func(t *testing.T) {
		sk1 := SessionFromContext(context.Background())
		sk2 := SessionFromContext(context.Background())
		if sk1.ClientID == "" || sk2.ClientID == "" {
			t.Fatalf("缺身份应得到非空 anon 桶，got %+v / %+v", sk1, sk2)
		}
		if sk1 == sk2 {
			t.Fatalf("两次缺身份调用不得共享同一桶（S-01）：%v", sk1)
		}
	})
	t.Run("ctx 带 user 身份 → 提取出 ClientID", func(t *testing.T) {
		id, err := requestidentity.User("client-A")
		if err != nil {
			t.Fatalf("User: %v", err)
		}
		ctx := requestidentity.WithIdentity(context.Background(), id)
		sk := SessionFromContext(ctx)
		if sk.ClientID != "client-A" || sk.System {
			t.Fatalf("应提取 user:client-A，got %+v", sk)
		}
		if sk.String() != "user:client-A" {
			t.Fatalf("String()=%q", sk.String())
		}
	})
	t.Run("ctx 带 system 身份 → 显式系统桶", func(t *testing.T) {
		ctx := requestidentity.SystemContext(context.Background())
		sk := SessionFromContext(ctx)
		if !sk.System || sk.ClientID != "" {
			t.Fatalf("应为显式系统桶，got %+v", sk)
		}
		if sk.String() != "system" {
			t.Fatalf("String()=%q", sk.String())
		}
		// 系统桶与用户桶互相隔离
		id, _ := requestidentity.User("client-B")
		userSk := SessionFromContext(requestidentity.WithIdentity(context.Background(), id))
		if userSk == sk {
			t.Fatalf("系统桶与用户桶不得相同")
		}
	})
	t.Run("user 身份缺主体（防御）→ 一次性独立桶", func(t *testing.T) {
		// WithIdentity 只该由 middleware 调用且 User() 已拒绝空主体；
		// 这里直接写零 ClientID 的 user 身份，验证消费方兜底不落共享桶。
		ctx := requestidentity.WithIdentity(context.Background(),
			requestidentity.Identity{Kind: requestidentity.KindUser})
		sk1 := SessionFromContext(ctx)
		sk2 := SessionFromContext(ctx)
		if sk1.System || sk1 == (SessionKey{}) || sk1 == sk2 {
			t.Fatalf("缺主体 user 身份应落到一次性独立桶，got %+v / %+v", sk1, sk2)
		}
	})
}

func TestTrackAndRelease(t *testing.T) {
	r := New()
	sk := SessionKey{ClientID: "c1"}

	ctx, release := r.Track(context.Background(), sk, 100, CatPlay)
	if r.Size(sk) != 1 {
		t.Fatalf("注册后桶大小应为 1，got %d", r.Size(sk))
	}
	if ctx.Err() != nil {
		t.Fatalf("刚 Track 的 ctx 不应已取消，err=%v", ctx.Err())
	}

	release()
	if r.Size(sk) != 0 {
		t.Fatalf("release 后桶应清空，got %d", r.Size(sk))
	}
	if ctx.Err() == nil {
		t.Fatalf("release 后 ctx 应被 cancel")
	}
	if r.TotalSize() != 0 {
		t.Fatalf("空桶应被回收，TotalSize=%d", r.TotalSize())
	}
}

func TestActivate_CancelsOtherSongsInSameSession(t *testing.T) {
	r := New()
	sk := SessionKey{ClientID: "c1"}

	// song 100 的 play、transcode、reassign
	ctx100Play, _ := r.Track(context.Background(), sk, 100, CatPlay)
	ctx100Tc, _ := r.Track(context.Background(), sk, 100, CatTranscode)
	ctx100Reassign, _ := r.Track(context.Background(), sk, 100, CatReassign)

	// song 200 的 play
	ctx200Play, _ := r.Track(context.Background(), sk, 200, CatPlay)

	// 切到 song 200 → 同会话所有 songID != 200 的 play/transcode/reassign 都 cancel；
	// 200 的 play 不动。（prefetch 的保活行为见 TestActivate_KeepsPrefetchForOtherSongs）
	r.Activate(sk, 200)

	// 等到 cancel 真正落到 ctx
	if !waitCanceled(ctx100Play) {
		t.Errorf("song 100 play 应被 cancel")
	}
	if !waitCanceled(ctx100Tc) {
		t.Errorf("song 100 transcode 应被 cancel")
	}
	if !waitCanceled(ctx100Reassign) {
		t.Errorf("song 100 reassign 应被 cancel")
	}
	if ctx200Play.Err() != nil {
		t.Errorf("song 200 play 不应被 cancel，err=%v", ctx200Play.Err())
	}
}

// TestActivate_KeepsPrefetchForOtherSongs 锁定 songloft-org/songloft#300 的修复：
// prefetch 是为「下一首」预热的，正在播放 song N（Activate(N)）时插件已为 N+1 发起 prefetch。
// Activate 绝不能取消非当前歌的 prefetch，否则每次切歌都杀掉刚发起的下一首预热转码，
// 真正播放 N+1 时只能实时转码，prefetch 形同虚设。
func TestActivate_KeepsPrefetchForOtherSongs(t *testing.T) {
	r := New()
	sk := SessionKey{ClientID: "c1"}

	// 正在播放 song 100；插件已为下一首 song 101 发起 prefetch（转码）。
	ctx100Play, _ := r.Track(context.Background(), sk, 100, CatPlay)
	ctx101Prefetch, _ := r.Track(context.Background(), sk, 101, CatPrefetch)

	// 切到 song 100（真实播放触发 Activate(100)）。
	r.Activate(sk, 100)

	if ctx100Play.Err() != nil {
		t.Errorf("当前歌 song 100 play 不应被 cancel，err=%v", ctx100Play.Err())
	}
	if ctx101Prefetch.Err() != nil {
		t.Errorf("下一首 song 101 的 prefetch 不应被 cancel（#300），err=%v", ctx101Prefetch.Err())
	}
	if r.Size(sk) != 2 {
		t.Errorf("play 与 prefetch 都应保留，桶大小应为 2，got %d", r.Size(sk))
	}
}

func TestActivate_KeepsAllSelfSongWork(t *testing.T) {
	r := New()
	sk := SessionKey{ClientID: "c1"}

	ctxPlay, _ := r.Track(context.Background(), sk, 100, CatPlay)
	ctxPrefetch, _ := r.Track(context.Background(), sk, 100, CatPrefetch)
	ctxTc, _ := r.Track(context.Background(), sk, 100, CatTranscode)

	// 真实播放 100 → 同曲的所有工作都保留：慢音源（B站）的 prefetch 在后台解析+缓存，
	// 掐掉会逼同步播放路从零重解析并被客户端 5s 断连判死（songloft#271）。
	r.Activate(sk, 100)

	if ctxPlay.Err() != nil {
		t.Errorf("同 song play 不应被 cancel，err=%v", ctxPlay.Err())
	}
	if ctxPrefetch.Err() != nil {
		t.Errorf("同 song prefetch 不应被 cancel，err=%v", ctxPrefetch.Err())
	}
	if ctxTc.Err() != nil {
		t.Errorf("同 song transcode 不应被 cancel，err=%v", ctxTc.Err())
	}
}

func TestActivate_DoesNotAffectOtherSessions(t *testing.T) {
	r := New()
	skA := SessionKey{ClientID: "client-A"}
	skB := SessionKey{ClientID: "client-B"}

	// Client A 在 song 100 跑 transcode
	ctxATc, _ := r.Track(context.Background(), skA, 100, CatTranscode)
	// Client B 在 song 200 跑 transcode
	ctxBTc, _ := r.Track(context.Background(), skB, 200, CatTranscode)

	// Client A 切到 song 101
	r.Activate(skA, 101)

	if !waitCanceled(ctxATc) {
		t.Errorf("Client A 自己的 song 100 transcode 应被 cancel")
	}
	if ctxBTc.Err() != nil {
		t.Errorf("Client B 的 song 200 transcode 不应被 cancel（跨会话隔离），err=%v", ctxBTc.Err())
	}
}

func TestActivate_EmptyBucketNoOp(t *testing.T) {
	r := New()
	// 不应 panic
	r.Activate(SessionKey{ClientID: "ghost"}, 999)
	if r.TotalSize() != 0 {
		t.Errorf("空 registry 不应残留 entry")
	}
}

func TestActivate_ParentCanceledStillCleansEntry(t *testing.T) {
	r := New()
	sk := SessionKey{ClientID: "c1"}

	parent, parentCancel := context.WithCancel(context.Background())
	_, release := r.Track(parent, sk, 100, CatPlay)
	parentCancel() // 父 ctx 取消会让派生 ctx 也 Done，但不会自动从 map 删

	if r.Size(sk) != 1 {
		t.Fatalf("父 ctx 取消不会自动清 entry（需要 release 或 Activate）")
	}
	release()
	if r.Size(sk) != 0 {
		t.Fatalf("release 后应清掉 entry")
	}
}

func TestRegistry_ConcurrentSafety(t *testing.T) {
	r := New()
	const goroutines = 32
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(workerID int) {
			defer wg.Done()
			sk := SessionKey{ClientID: "worker"}
			for j := range iterations {
				songID := int64(workerID*iterations + j)
				_, release := r.Track(context.Background(), sk, songID, CatPlay)
				if j%5 == 0 {
					r.Activate(sk, songID)
				}
				release()
			}
		}(i)
	}
	wg.Wait()

	if r.TotalSize() != 0 {
		t.Errorf("所有 release 后 registry 应为空，TotalSize=%d", r.TotalSize())
	}
}

// waitCanceled 等到 ctx.Done()（最多 100ms）。Activate 内部 cancel 是同步触发的，
// 但 ctx.Err() 的 visibility 通过内部 close(ctx.Done()) 完成；借短轮询保证测试稳定。
func waitCanceled(ctx context.Context) bool {
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return ctx.Err() != nil
}
