package managed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestTranscodeToMP3_RealAACFixture：真实 AAC 样本走完整规范化——
// 转码产物必须通过 VerifyMP3（codec/帧/码率/时长全检）。
// 依赖 ffmpeg 的环境门槛由 runner 把关（缺 ffmpeg = BLOCKED 而非跳过）。
func TestTranscodeToMP3_RealAACFixture(t *testing.T) {
	out, err := TranscodeToMP3(context.Background(),
		filepath.Join("testdata", "audio.m4a"), t.TempDir(), TranscoderLimits{})
	if err != nil {
		t.Fatalf("AAC→MP3 转码失败: %v", err)
	}
	if filepath.Base(out) != "intermediate.mp3" {
		t.Errorf("产物必须是中间名（不得直接冒充最终名），got %s", out)
	}
	rep, err := VerifyMP3(out, VerifyExpectation{MaxBitrateKbps: 320})
	if err != nil {
		t.Fatalf("转码产物未通过完整验证: %v", err)
	}
	if rep.FrameCount == 0 || rep.DurationEstSec < 1.0 {
		t.Errorf("产物帧事实异常: %+v", rep)
	}
}

// TestTranscodeToMP3_RejectsGarbageInput：非媒体输入失败且不留产物。
func TestTranscodeToMP3_RejectsGarbageInput(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.m4a")
	if err := os.WriteFile(bad, []byte("definitely not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if _, err := TranscodeToMP3(context.Background(), bad, outDir, TranscoderLimits{Timeout: 30_000_000_000}); err == nil {
		t.Fatal("垃圾输入应失败")
	}
	if _, err := os.Stat(filepath.Join(outDir, "intermediate.mp3")); !os.IsNotExist(err) {
		t.Error("失败不得留下产物")
	}
}
