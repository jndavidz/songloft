package managed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"songloft/internal/database/testutil"
	"songloft/internal/middleware"
	"songloft/internal/models"
	"songloft/internal/services"
)

const testJWTSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestBFF_HandoffStateMachine：两段式 handoff——不确认 Stop 就不能播（防双播）。
func TestBFF_HandoffStateMachine(t *testing.T) {
	bff := NewManagedBFF(nil)
	ctx := context.Background()

	// 队列编辑在任何阶段允许
	if err := bff.SetQueue("s1", 1, []QueueEntry{{Entry: "e1", RefDigest: "d1"}}); err != nil {
		t.Fatalf("set queue: %v", err)
	}
	// native 阶段播放命令拒绝（手机原生续播职责还在，防双 owner）
	if err := bff.Execute(ctx, "s1", Command{Name: "play", Epoch: 1, Entry: "e1"}); err == nil {
		t.Fatal("native 阶段不得接受播放命令")
	}

	// 第一步：请求接管
	phase, err := bff.RequestHandoff("s1")
	if err != nil || phase != PhaseHandoffPending {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
	// pending 阶段播放命令仍拒绝（旧续播职责未确认结束）
	if err := bff.Execute(ctx, "s1", Command{Name: "play", Epoch: 1, Entry: "e1"}); err == nil {
		t.Fatal("handoff-pending 阶段不得接受播放命令（可能双播）")
	}
	// stop 任何阶段都接受
	if err := bff.Execute(ctx, "s1", Command{Name: "stop", Epoch: 1, Entry: "e1"}); err != nil {
		t.Fatalf("stop 应总是接受: %v", err)
	}
	// 第二步：显式 Stop 确认 → managed
	if err := bff.ConfirmHandoffStop("s1", 1); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := bff.Execute(ctx, "s1", Command{Name: "play", Epoch: 1, Entry: "e1"}); err != nil {
		t.Fatalf("managed 阶段应接受播放: %v", err)
	}
}

// TestBFF_StaleEpochCommands：旧 epoch 命令拒绝；换代后旧 App 不自动恢复 owner。
func TestBFF_StaleEpochCommands(t *testing.T) {
	bff := NewManagedBFF(nil)
	ctx := context.Background()
	_ = bff.SetQueue("s1", 1, []QueueEntry{{Entry: "e1", RefDigest: "d1"}})
	_, _ = bff.RequestHandoff("s1")
	_ = bff.ConfirmHandoffStop("s1", 1)

	newEpoch := bff.NewEpoch("s1")
	if newEpoch != 2 {
		t.Fatalf("newEpoch=%d", newEpoch)
	}
	if err := bff.Execute(ctx, "s1", Command{Name: "play", Epoch: 1, Entry: "e1"}); err == nil {
		t.Fatal("旧 epoch 命令必须拒绝")
	}
	if err := bff.SetQueue("s1", 1, nil); err == nil {
		t.Fatal("旧 epoch 队列编辑必须拒绝")
	}
	// 新 epoch 生效后旧 App 的确认也无效
	if err := bff.ConfirmHandoffStop("s1", 1); err == nil {
		t.Fatal("旧 epoch 的 handoff 确认必须拒绝")
	}
}

// TestEvents_IdempotencyAndBound：幂等去重、不记已听白名单、replay、确认清理。
func TestEvents_IdempotencyAndBound(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	log := NewEventLog(mdb.DB())
	ctx := context.Background()

	t.Run("幂等键重复只记一次", func(t *testing.T) {
		ev := &ListenEvent{Type: EventPlayed, RefDigest: "d", Entry: "e",
			LeaseID: "l", Session: "s", IdempotencyKey: "play-e1"}
		seq1, err := log.Append(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		seq2, err := log.Append(ctx, ev) // 同键重投
		if err != nil {
			t.Fatal(err)
		}
		if seq1 != seq2 {
			t.Errorf("重复投递应返回既有 seq，got %d vs %d", seq1, seq2)
		}
		pending, _ := log.Pending(ctx, 0, 100)
		n := 0
		for _, p := range pending {
			if p.IdempotencyKey == "play-e1" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("历史不得重复，got %d 条", n)
		}
	})
	t.Run("prepare/prefetch 类型不得记已听", func(t *testing.T) {
		for _, typ := range []string{"prepare", "range", "head", "prefetch"} {
			if _, err := log.Append(ctx, &ListenEvent{Type: typ, IdempotencyKey: "k-" + typ}); err == nil {
				t.Errorf("类型 %q 不得进入已听历史", typ)
			}
		}
	})
	t.Run("缺幂等键拒绝", func(t *testing.T) {
		if _, err := log.Append(ctx, &ListenEvent{Type: EventPlayed}); err == nil {
			t.Fatal("缺幂等键应拒绝")
		}
	})
	t.Run("Last-Event-ID replay 与 Ack 清理", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			if _, err := log.Append(ctx, &ListenEvent{Type: EventProgress,
				IdempotencyKey: fmt.Sprintf("p-%d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		// 从 seq 2 之后 replay（模拟崩溃/断连补偿）
		pending, err := log.Pending(ctx, 2, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 || pending[0].Seq <= 2 {
			t.Fatalf("replay 应从 seq>2 开始，got %d 条首 seq=%d", len(pending), pending[0].Seq)
		}
		// Ack 到最新，再 replay 为空
		maxSeq, _ := log.MaxSeq(ctx)
		if err := log.Ack(ctx, maxSeq); err != nil {
			t.Fatal(err)
		}
		pending, _ = log.Pending(ctx, 0, 100)
		if len(pending) != 0 {
			t.Errorf("Ack 后 pending 应为空，got %d", len(pending))
		}
	})
}

// TestMapAlbum_FourSourcesIdentity：四源身份逐项保留；缺字段显式失败可见。
func TestMapAlbum_FourSourcesIdentity(t *testing.T) {
	sourceDatas := []string{
		`{"s":"kwqq","i":"1001","n":"a","a":"b","al":"c"}`, // kwqq 无需 extra
		`{"s":"migu","i":"m1","c":"690001"}`,               // migu 带版权 ID
		`{"s":"bilibili","i":"BV1x","cid":30251,"page":2}`, // B站带分 P
		`{"s":"migu","i":"m2"}`,                            // 缺 copyrightId → 显式失败
		`not-json`,                                         // 畸形 → 显式失败
	}
	titles := []string{"t1", "t2", "t3", "t4", "t5"}
	out, err := MapAlbum(sourceDatas, titles, []string{"kwqq", "migu", "bilibili"})
	if err != nil {
		t.Fatalf("部分失败不得吞整专辑: %v", err)
	}
	if out[0].Ref == nil || out[0].Ref.ID != "1001" {
		t.Errorf("kwqq 项身份缺失: %+v", out[0])
	}
	if out[1].Ref == nil || out[1].Ref.Extra["copyrightId"] != "690001" {
		t.Errorf("migu copyrightId 必须保留: %+v", out[1])
	}
	if out[2].Ref == nil || out[2].Ref.Extra["cid"] != "30251" || out[2].Ref.Extra["page"] != 2 {
		t.Errorf("B站 cid/page 必须保留: %+v", out[2])
	}
	if out[3].Ref != nil || out[3].Error == "" {
		t.Errorf("缺 copyrightId 的项必须显式失败且可见: %+v", out[3])
	}
	if out[4].Ref != nil || out[4].Error == "" {
		t.Errorf("畸形项必须显式失败: %+v", out[4])
	}
	// 全部失败 → 整专辑错误（可见，不是静默消失）
	_, err = MapAlbum([]string{`not-json`, `{}`}, nil, []string{"kwqq"})
	if err == nil {
		t.Fatal("全部失败应返回整专辑错误")
	}
}

// TestBFF_RequiresAuth：BFF 入口不得加入 publicPaths——无 token 必须 401。
// 用真实 AuthMiddleware 包裹 BFF handler 桩，验证鉴权面接线（与 S-01 接线测试同法）。
func TestBFF_RequiresAuth(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	if err := mdb.ConfigRepository().Set(context.Background(), &models.Config{
		Key: "jwt_secret", Value: testJWTSecret,
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	authSvc, err := services.NewAuthService(mdb.ConfigRepository(), mdb.TokenRepository(), "admin", "password")
	if err != nil {
		t.Fatalf("auth svc: %v", err)
	}
	_ = NewManagedBFF(nil) // BFF handler 的生产接线在 app 层；此处验证鉴权面语义
	handler := middleware.AuthMiddleware(authSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/managed/queue", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 token 的 BFF 请求必须 401（不得进 publicPaths），got %d", resp.StatusCode)
	}
}
