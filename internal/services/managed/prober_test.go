package managed

import (
	"os"
	"testing"
)

// TestDefaultProber_ValidFixture：环境有 ffprobe 时，真 fixture 交叉验证
// 应得 codec=mp3 且时长与帧估计一致（走 public VerifyMP3 seam）。
func TestDefaultProber_ValidFixture(t *testing.T) {
	prober := DefaultProber()
	if prober == nil {
		t.Skip("ffprobe 不在 PATH，跳过（生产容器应自带）")
	}
	rep, err := VerifyMP3(fixturePath(t, "valid_128k.mp3"),
		VerifyExpectation{MaxBitrateKbps: 320, Prober: prober})
	if err != nil {
		t.Fatalf("带 Prober 的验证应通过: %v", err)
	}
	if rep.ProbeCodec != "mp3" {
		t.Errorf("ProbeCodec 应为 mp3，got %q", rep.ProbeCodec)
	}
	if rep.ProbeDurationSec <= 0 {
		t.Errorf("ProbeDurationSec 应 > 0，got %.3f", rep.ProbeDurationSec)
	}
}

// TestDefaultProber_MissingBinary：PATH 清空气应返回 nil（降级跳过），不 panic。
func TestDefaultProber_MissingBinary(t *testing.T) {
	old := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent-for-prober-test")
	defer t.Setenv("PATH", old)
	if got := DefaultProber(); got != nil {
		t.Error("PATH 无 ffprobe 时 DefaultProber 应返回 nil")
	}
}
