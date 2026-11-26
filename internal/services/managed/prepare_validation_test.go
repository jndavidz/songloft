package managed

import (
	"testing"
	"time"
)

// TestPrepare_ValidationLevelReachesOp：三态验证结果必须写入 PrepareOp，
// 才能被票据/诊断面读取（Standards 审查 Major-2：避免死数据）。
func TestPrepare_ValidationLevelReachesOp(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}}
	m := NewManager(testDeps(t, res))
	ref := kwqqRef("validation-level-1")
	op, err := m.StartPrepare("songloft-managed",
		OpKey{Session: "sess-v", Epoch: 1, Entry: "e-v"}, ref)
	if err != nil {
		t.Fatalf("StartPrepare: %v", err)
	}
	select {
	case <-op.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("prepare 超时")
	}
	q, ok := m.Query("songloft-managed", OpKey{Session: "sess-v", Epoch: 1, Entry: "e-v"})
	if !ok || q.State != StateReady {
		t.Fatalf("应 ready，got %+v ok=%v（resolver=%d）", q, ok, res.count())
	}
	if q.ValidationLevel != "verified" && q.ValidationLevel != "degraded" {
		t.Errorf("ValidationLevel 应为 verified/degraded，got %q", q.ValidationLevel)
	}
	if q.ValidationReason == "" {
		t.Error("ValidationReason 不应为空")
	}
}
