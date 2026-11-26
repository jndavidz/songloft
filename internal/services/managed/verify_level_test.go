package managed

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hermetic 字节夹具：用仓库自带真 MP3 构造各尾部场景，全部在 t.TempDir 内，
// 不依赖外部 tmp 路径（所有 agent 环境可跑）。
func writeVerifyFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func validMP3Bytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(t, "valid_128k.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVerifyLevel_RealWorld37BDegraded：完整帧流 + 37B 无标签零填充残片
// 不得误杀（真机《爱你一万年》根因），应为 degraded。
func TestVerifyLevel_RealWorld37BDegraded(t *testing.T) {
	p := writeVerifyFixture(t, "benign37b.mp3",
		append(append([]byte(nil), validMP3Bytes(t)...), make([]byte, 37)...))
	rep, err := VerifyMP3(p, VerifyExpectation{MaxBitrateKbps: 320})
	if err != nil {
		t.Fatalf("37B 无标签残片不得被拒绝: %v", err)
	}
	if rep.ValidationLevel != "degraded" {
		t.Errorf("37B 残片应为 degraded（精确断言，不接受 verified），got %q", rep.ValidationLevel)
	}
	if rep.TrailingJunkBytes != 37 {
		t.Errorf("TrailingJunkBytes 应为 37，got %d", rep.TrailingJunkBytes)
	}
	if !strings.Contains(rep.ValidationReason, verifierVersion) {
		t.Errorf("原因应含验证器版本，got %q", rep.ValidationReason)
	}
}

// TestVerifyLevel_RealSample_WhenAvailable：真机《爱你一万年》原文件（37B 残片）
// 按 env 可达时跑 public seam；不可达显式 Skip（不静默降级为 hermetic 替代）。
func TestVerifyLevel_RealSample_WhenAvailable(t *testing.T) {
	p := os.Getenv("SONGLOFT_VERIFY_REAL_SAMPLE")
	if p == "" {
		t.Skip("设 SONGLOFT_VERIFY_REAL_SAMPLE 指向真机样本")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("真机样本不可达: %v", err)
	}
	rep, err := VerifyMP3(p, VerifyExpectation{MaxBitrateKbps: 320})
	if err != nil {
		t.Fatalf("真机 37B 残片样本不得被拒绝: %v", err)
	}
	if rep.ValidationLevel != "degraded" {
		t.Errorf("真机样本应为 degraded，got %q", rep.ValidationLevel)
	}
	if rep.FrameCount < 5000 {
		t.Errorf("真机样本应有完整帧流（>5000），got %d", rep.FrameCount)
	}
}

// TestVerifyLevel_LeakKillRejected：四类漏杀样本必须拒绝（hermetic 构造）。
func TestVerifyLevel_LeakKillRejected(t *testing.T) {
	base := validMP3Bytes(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"末帧越过EOF", base[:len(base)-4]}, // 截断末帧尾部 ⇒ 声明帧长越过 EOF
		{"128KiB垃圾含TAG", append(append(append([]byte(nil), base...), bytes.Repeat([]byte{'X'}, 128*1024-3)...), []byte("TAG")...)},
		{"128KiB垃圾末尾ID3v1伪装", append(append(append([]byte(nil), base...), bytes.Repeat([]byte{0x55}, 128*1024-128)...), append([]byte("TAG"), make([]byte, 125)...)...)},
		{"末帧只剩3字节", base[:len(base)-3]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeVerifyFixture(t, tc.name+".mp3", tc.data)
			rep, err := VerifyMP3(p, VerifyExpectation{MaxBitrateKbps: 320})
			if err == nil {
				t.Fatalf("%s 必须被拒绝（漏杀防线），got level=%q", tc.name, rep.ValidationLevel)
			}
			if rep == nil || rep.ValidationLevel != "rejected" {
				t.Fatalf("拒绝报告必须携带 rejected 等级，got %+v", rep)
			}
		})
	}
}

// TestVerifyLevel_RejectedReportCarriesLevel：拒绝时仍返回 rep（三态契约）。
func TestVerifyLevel_RejectedReportCarriesLevel(t *testing.T) {
	rep, err := VerifyMP3(fixturePath(t, "not_audio.mp3"), VerifyExpectation{MaxBitrateKbps: 320})
	if err == nil {
		t.Fatal("非音频必须被拒绝")
	}
	if rep == nil || rep.ValidationLevel != "rejected" || rep.ValidationReason == "" {
		t.Fatalf("拒绝报告必须带 rejected + 原因，got %+v", rep)
	}
}

// TestVerifyLevel_ValidIsVerified：干净 fixture 仍是完全验证（不降级）。
func TestVerifyLevel_ValidIsVerified(t *testing.T) {
	rep, err := VerifyMP3(fixturePath(t, "valid_128k.mp3"), VerifyExpectation{MaxBitrateKbps: 320})
	if err != nil {
		t.Fatalf("干净 fixture 应通过: %v", err)
	}
	if rep.ValidationLevel != "verified" {
		t.Errorf("干净 fixture 应为 verified，got %q", rep.ValidationLevel)
	}
}
