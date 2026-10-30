package managed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitPrepare 等 op 进入终态并返回查询快照。
func waitPrepare(t *testing.T, m *Manager, issuer string, key OpKey) *PrepareOp {
	t.Helper()
	op, err := m.StartPrepare(issuer, key, MustRef(t, "kwqq", "1", nil))
	if err != nil {
		t.Fatalf("start prepare: %v", err)
	}
	select {
	case <-op.Done:
	case <-time.After(3 * time.Second):
		t.Fatalf("prepare did not finish")
	}
	snap, ok := m.Query(issuer, key)
	if !ok {
		t.Fatalf("query returned no op")
	}
	return snap
}

// TestPrepare_InterruptedDownloadFailsVisible：下载中断（已写半文件后报错）必须
// 失败可见、不产出 final、不带票据；半文件留在隔离目录里，绝不进正式表示。
func TestPrepare_InterruptedDownloadFailsVisible(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}}
	deps := testDeps(t, res)
	deps.Downloader = func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error) {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return "", 0, err
		}
		partial := filepath.Join(destDir, "source.mp3")
		if err := os.WriteFile(partial, []byte("half-a-song"), 0o644); err != nil {
			return "", 0, err
		}
		return partial, 11, fmt.Errorf("network interrupted mid-download")
	}
	m := NewManager(deps)
	key := OpKey{Session: "s", Epoch: 1, Entry: "e"}
	snap := waitPrepare(t, m, "i", key)
	if snap.State != StateFailed {
		t.Fatalf("state=%s want failed", snap.State)
	}
	if snap.Error == "" {
		t.Fatal("失败必须带可见原因")
	}
	digest, _ := MustRef(t, "kwqq", "1", nil).Digest()
	if _, err := os.Stat(filepath.Join(deps.WorkDir, digest, "final.mp3")); err == nil {
		t.Fatal("中断的下载不得产出 final.mp3")
	}
}

// TestPrepare_WriteFailureFailsVisible：表示目录不可写（磁盘满/权限）时同样
// 失败可见，不产出 final，也不把原始字节当成品。
func TestPrepare_WriteFailureFailsVisible(t *testing.T) {
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}}
	deps := testDeps(t, res)
	ref := MustRef(t, "kwqq", "1", nil)
	digest, _ := ref.Digest()
	// 用同名普通文件占住表示目录，使 MkdirAll 失败（等效「目录不可写」）。
	if err := os.WriteFile(filepath.Join(deps.WorkDir, digest), []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(deps)
	key := OpKey{Session: "s", Epoch: 1, Entry: "e"}
	snap := waitPrepare(t, m, "i", key)
	if snap.State != StateFailed {
		t.Fatalf("state=%s want failed", snap.State)
	}
	if snap.Error == "" {
		t.Fatal("失败必须带可见原因")
	}
	if _, err := os.Stat(filepath.Join(deps.WorkDir, digest, "final.mp3")); err == nil {
		t.Fatal("写入失败不得产出 final.mp3")
	}
}
