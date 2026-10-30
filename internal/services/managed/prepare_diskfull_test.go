//go:build linux

package managed

// 受控故障注入：真实 ENOSPC 下的准备链。
//
// 宿主机（Windows/WSL/NAS）没有 root，不能 losetup/mount 真 loop 分区，所以本文件
// 只在 Linux 上编译，并要求 TMPDIR 指向一个小容量 tmpfs（例如 docker 的
// `--tmpfs /work:size=2m`）。这样 t.TempDir() 与 PrepareDeps.WorkDir 都落在同一块
// 小文件系统上，写入超容量时得到的就是内核真实返回的 ENOSPC，而不是伪造的错误。
//
// 运行方式见本轮验收报告：
//
//	docker run --rm --tmpfs /work:size=2m -e TMPDIR=/work/tmp -e GOTMPDIR=/gotmp ...
//
// 注意：GOCACHE/GOTMPDIR 必须放在 tmpfs 之外，否则 Go 自己先写爆。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	diskFullProbeBytes = 8 << 20 // 8MB，远超 2MB tmpfs
	diskFullChunkBytes = 256 << 10
)

// requireDiskFullEnv 把小容量 tmpfs 变成**显式开关**：普通开发/CI 环境
// （没有小 tmpfs）必须跳过，而不是把「环境不具备」误报成产品失败。
func requireDiskFullEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("SONGLOFT_DISKFULL_TMPFS") != "1" {
		t.Skip("disk-full 注入需要小容量 tmpfs：" +
			"SONGLOFT_DISKFULL_TMPFS=1 且 TMPDIR/GOTMPDIR 指向 tmpfs（见文件头）")
	}
}

// writeUntilFull 往 path 持续写 chunk，直到内核返回错误；返回已写字节数与错误。
// 这是「真实写入失败」的唯一来源——不构造假 error。
func writeUntilFull(path string) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	chunk := bytes.Repeat([]byte{0x5a}, diskFullChunkBytes)
	var written int64
	for i := 0; i < 64; i++ { // 上限 16MB，足够打满 2MB tmpfs
		n, err := f.Write(chunk)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, fmt.Errorf("tmpfs 未被写满：写入 %d 字节仍成功，测试环境不可信", written)
}

// TestDiskFull_ProbeENOSPC 是「环境可用」探针：向 TMPDIR 写 8MB 必须拿到真实 ENOSPC。
// 探针失败说明 TMPDIR 没落在小容量挂载上，后续准备链结论不可信。
func TestDiskFull_ProbeENOSPC(t *testing.T) {
	requireDiskFullEnv(t)
	dir := t.TempDir()
	t.Logf("probe dir (TMPDIR=%s): %s", os.Getenv("TMPDIR"), dir)
	path := filepath.Join(dir, "probe-8mb.bin")
	written, err := writeUntilFull(path)
	t.Logf("probe wrote %d bytes before error: %v", written, err)
	if err == nil {
		t.Fatal("probe 未触发 ENOSPC：测试环境不是小容量 tmpfs")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("probe err = %v, want ENOSPC", err)
	}
	if written >= diskFullProbeBytes {
		t.Fatalf("probe wrote %d bytes >= %d, tmpfs 容量不符预期", written, diskFullProbeBytes)
	}
}

// TestPrepare_DiskFullDuringDownloadFailsVisible：下载阶段真实写入 ENOSPC 时，
// 准备链必须失败可见、不产出 final.mp3；半文件留在隔离目录，绝不进正式表示。
func TestPrepare_DiskFullDuringDownloadFailsVisible(t *testing.T) {
	requireDiskFullEnv(t)
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}}
	deps := testDeps(t, res) // WorkDir = t.TempDir()，与小 tmpfs 上的 TMPDIR 同盘
	var partialPath string
	deps.Downloader = func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error) {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return "", 0, err
		}
		partialPath = filepath.Join(destDir, "source.mp3")
		written, err := writeUntilFull(partialPath)
		if err == nil {
			return "", 0, fmt.Errorf("downloader 未触发 ENOSPC：测试环境不可信")
		}
		// 与真实下载器同口径：把底层写入错误原样上抛（不吞、不转成「成功」）。
		return partialPath, written, err
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
	if !strings.Contains(strings.ToLower(snap.Error), "no space left on device") {
		t.Fatalf("error 必须暴露真实写入失败原因，got %q", snap.Error)
	}
	digest, _ := MustRef(t, "kwqq", "1", nil).Digest()
	final := filepath.Join(deps.WorkDir, digest, "final.mp3")
	if _, err := os.Stat(final); err == nil {
		t.Fatal("ENOSPC 下载不得产出 final.mp3")
	}
	// 半文件只能留在隔离目录里（供重试/GC），绝不冒充成品。
	if info, err := os.Stat(partialPath); err != nil || info.Size() == 0 {
		t.Fatalf("半文件应留在隔离目录: info=%v err=%v", info, err)
	}
}

// TestPrepare_DiskFullDuringPromoteFailsVisible：下载与验证都成功，但「验证通过后
// 落位 final.mp3」这一步撞到 ENOSPC —— 同样必须失败可见、不留 0 字节/半截 final，
// 且原始 source 保留（表示隔离，原码不丢）。
func TestPrepare_DiskFullDuringPromoteFailsVisible(t *testing.T) {
	requireDiskFullEnv(t)
	res := &countingResolver{next: func(req ResolveRequest) (*ResolveResult, error) {
		return okResult(t, false), nil
	}}
	deps := testDeps(t, res)
	var srcPath string
	deps.Downloader = func(ctx context.Context, result *ResolveResult, destDir string) (string, int64, error) {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return "", 0, err
		}
		src, err := os.Open(filepath.Join("testdata", "valid_128k.mp3"))
		if err != nil {
			return "", 0, err
		}
		defer src.Close()
		dstPath := filepath.Join(destDir, "source.mp3")
		dst, err := os.Create(dstPath)
		if err != nil {
			return "", 0, err
		}
		n, err := io.Copy(dst, src)
		cerr := dst.Close()
		if err != nil {
			return "", 0, err
		}
		if cerr != nil {
			return "", 0, cerr
		}
		// 下载本身成功；把 tmpfs 填满，让随后的 final 落位（copyFile）真实 ENOSPC。
		if _, err := writeUntilFull(filepath.Join(deps.WorkDir, "filler.bin")); err == nil {
			return "", 0, fmt.Errorf("未能填满 tmpfs：测试环境不可信")
		}
		srcPath = dstPath
		return dstPath, n, nil
	}
	m := NewManager(deps)
	key := OpKey{Session: "s", Epoch: 1, Entry: "e"}
	snap := waitPrepare(t, m, "i", key)

	if snap.State != StateFailed {
		t.Fatalf("state=%s want failed（落位失败不得报 ready）", snap.State)
	}
	if !strings.Contains(strings.ToLower(snap.Error), "no space left on device") {
		t.Fatalf("error 必须暴露 ENOSPC，got %q", snap.Error)
	}
	digest, _ := MustRef(t, "kwqq", "1", nil).Digest()
	final := filepath.Join(deps.WorkDir, digest, "final.mp3")
	if _, err := os.Stat(final); err == nil {
		t.Fatal("落位失败不得留下 final.mp3")
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Fatalf("原始 source 不得被删除（表示隔离）: %v", err)
	}
}
