package managed

import (
	"context"
	"testing"

	"songloft/internal/database/testutil"
)

// TestStop_RevokesByTicketEpoch（T0 真因#1 回归）：
// stop 必须按票据签发时的 epoch 撤销，不受 BFF session 本地代次影响。
// verify06 现场：票据 epoch=4，但 session 从未初始化到 epoch 4（默认 1），
// 旧实现在 stale-epoch 前置检查处就拒绝了 stop，票据零撤销。
func TestStop_RevokesByTicketEpoch(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	bff := NewManagedBFF(iss)
	ctx := context.Background()

	// 票据按 prepare 请求的 epoch=4 签发（session=probe06, entry=q1）
	tk, err := iss.Issue(ctx, TicketRequest{
		RefDigest: "d", Representation: "managed/x/final.mp3",
		Session: "probe06", Epoch: 4, Entry: "q1", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	// stop 携带票据的 epoch=4；BFF session 默认 epoch=1
	if err := bff.Execute(ctx, "probe06", Command{Name: "stop", Epoch: 4, Entry: "q1"}); err != nil {
		t.Fatalf("stop 不得被 stale epoch 拒绝（应按票据 epoch 撤销）: %v", err)
	}
	rec, err := iss.q.GetMediaTicketByHash(ctx, hashToken(tk.Token))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Revoked != 1 {
		t.Errorf("stop 后 DB revoked 应为 1（撤销落库）, got %d", rec.Revoked)
	}
}

// TestStop_RevokesOnlyTargetEntry（entry 键语义回归）：
// stop 只撤销指定 entry 的票据，不误伤同 session 其他 entry。
func TestStop_RevokesOnlyTargetEntry(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	bff := NewManagedBFF(iss)
	ctx := context.Background()

	tkTarget, _ := iss.Issue(ctx, TicketRequest{
		RefDigest: "d1", Representation: "managed/x/final.mp3",
		Session: "s", Epoch: 1, Entry: "e1", Target: "device", Subject: "sub",
	})
	tkOther, _ := iss.Issue(ctx, TicketRequest{
		RefDigest: "d2", Representation: "managed/y/final.mp3",
		Session: "s", Epoch: 1, Entry: "e2", Target: "device", Subject: "sub",
	})
	if err := bff.Execute(ctx, "s", Command{Name: "stop", Epoch: 1, Entry: "e1"}); err != nil {
		t.Fatal(err)
	}
	r1, _ := iss.q.GetMediaTicketByHash(ctx, hashToken(tkTarget.Token))
	r2, _ := iss.q.GetMediaTicketByHash(ctx, hashToken(tkOther.Token))
	if r1.Revoked != 1 {
		t.Error("目标 entry=e1 应被撤销")
	}
	if r2.Revoked != 0 {
		t.Error("非目标 entry=e2 不得被误撤")
	}
}

// TestStop_WrongEpochDoesNotRevokeOtherEpoch（审查 Q2 边界固化）：
// stop 豁免 stale-epoch 后，撤销仍严格限定 (session, cmd.Epoch, cmd.Entry) 三元组，
// 携带错误 epoch 的 stop 不得误撤其他代次的票据——这是"stop 按票据 epoch 撤销"
// 不会造成跨代次误撤/DoS 的安全边界。
func TestStop_WrongEpochDoesNotRevokeOtherEpoch(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	bff := NewManagedBFF(iss)
	ctx := context.Background()

	tk, _ := iss.Issue(ctx, TicketRequest{
		RefDigest: "d", Representation: "managed/x/final.mp3",
		Session: "s", Epoch: 4, Entry: "e1", Target: "device", Subject: "sub",
	})
	// stop 用 epoch=3（错）+ 同 entry——不得撤 epoch=4 的票据
	if err := bff.Execute(ctx, "s", Command{Name: "stop", Epoch: 3, Entry: "e1"}); err != nil {
		t.Fatal(err)
	}
	rec, _ := iss.q.GetMediaTicketByHash(ctx, hashToken(tk.Token))
	if rec.Revoked != 0 {
		t.Error("错误 epoch 的 stop 不得撤销其他代次票据（三元组精确匹配）")
	}
}

// TestRevokeEntry_ImmediateBroadcast（T0 真因#3 回归）：
// RevokeEntry 必须像 RevokeLease 一样即时广播 token hash，
// 使在途流下一次 Read 即断，而非等 1s DB 轮询（快传 <1s 时 guard 零触发）。
func TestRevokeEntry_ImmediateBroadcast(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	iss := NewTicketIssuer(mdb.DB())
	ctx := context.Background()
	globalRevocations = &RevocationRegistry{revoked: map[string]struct{}{}}

	tk, err := iss.Issue(ctx, TicketRequest{
		RefDigest: "d", Representation: "managed/x/final.mp3",
		Session: "s", Epoch: 1, Entry: "e1", Target: "device", Subject: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := hashToken(tk.Token)
	if globalRevocations.has(h) {
		t.Fatal("撤销前 token 不应在广播表")
	}
	if err := iss.RevokeEntry(ctx, "s", 1, "e1"); err != nil {
		t.Fatal(err)
	}
	if !globalRevocations.has(h) {
		t.Error("RevokeEntry 后应立即广播 token hash（在途流秒断），当前缺失")
	}
}
