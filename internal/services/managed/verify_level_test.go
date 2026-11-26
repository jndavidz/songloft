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

// 真机样本默认路径（工作区约定）；env 可覆盖。存在即跑，不存在 Skip（B-1）。
func realSamplePath() string {
	if p := os.Getenv("SONGLOFT_VERIFY_REAL_SAMPLE"); p != "" {
		return p
	}
	return "/mnt/d/_work/dev/tmp/open-xiaoai/20261127/verifymp3/ai-ni-yi-wan-nian.source.mp3"
}

// TestVerifyLevel_RealSample：真机《爱你一万年》原文件（sha256 efa14216…，
// 5623 帧 + 37B 残片）走 public seam，应为 degraded（ticket 01 判据 1）。
func TestVerifyLevel_RealSample(t *testing.T) {
	p := realSamplePath()
	if _, err := os.Stat(p); err != nil {
		t.Skipf("真机样本不可达（设 SONGLOFT_VERIFY_REAL_SAMPLE）: %v", err)
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

// 六组复核探针的 hermetic 精确复现（B-2）：场景语义与 dev: tmp/.../verifymp3/
// 的六个文件一一对应，含此前漏测的 3B TAG 与帧头碎片（hasFrameSyncIn 防线）。
func verifyProbeCases(base []byte) []struct {
	name string
	data []byte
	want string // "verified" / "degraded" / "rejected"
} {
	// 末帧起点：从尾部往前找最后一个 0xFFE 同步点。
	last := -1
	for i := len(base) - 4; i >= 0; i-- {
		if base[i] == 0xFF && base[i+1]&0xE0 == 0xE0 {
			last = i
			break
		}
	}
	return []struct {
		name string
		data []byte
		want string
	}{
		{"完整帧流", base, "verified"},
		{"末帧只剩4字节头", base[:last+4], "rejected"},
		{"末帧只剩3字节(帧头碎片)", base[:last+3], "rejected"},
		{"3字节TAG尾部", append(append([]byte(nil), base...), []byte("TAG")...), "degraded"},
		{"128KiB垃圾含TAG", append(append(append([]byte(nil), base...), bytes.Repeat([]byte{'X'}, 128*1024-3)...), []byte("TAG")...), "rejected"},
		{"128KiB+1垃圾含TAG", append(append(append(append([]byte(nil), base...), bytes.Repeat([]byte{'X'}, 128*1024-3)...), []byte("TAG")...), 'X'), "rejected"},
	}
}

func TestVerifyLevel_SixProbesHermetic(t *testing.T) {
	base := validMP3Bytes(t)
	for _, tc := range verifyProbeCases(base) {
		t.Run(tc.name, func(t *testing.T) {
			p := writeVerifyFixture(t, tc.name+".mp3", tc.data)
			rep, err := VerifyMP3(p, VerifyExpectation{MaxBitrateKbps: 320})
			got := "verified"
			if err != nil {
				got = "rejected"
			} else if rep.ValidationLevel == "degraded" {
				got = "degraded"
			}
			if got != tc.want {
				t.Errorf("%s: 期望 %s，got %s (err=%v)", tc.name, tc.want, got, err)
			}
		})
	}
}

// 真探针文件按 env 可达时额外复核（不静默降级为 hermetic 替代）。
func TestVerifyLevel_OriginalProbeFiles(t *testing.T) {
	dir := os.Getenv("SONGLOFT_VERIFY_PROBE_DIR")
	if dir == "" {
		dir = "/mnt/d/_work/dev/tmp/open-xiaoai/20261126/verifymp3"
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("复核探针目录不可达: %v", err)
	}
	cases := map[string]string{
		"valid_fixture.mp3":                 "verified",
		"last_frame_header_only.mp3":        "rejected",
		"last_frame_three_bytes.mp3":        "rejected",
		"three_byte_TAG_tail.mp3":           "degraded",
		"128KiB_junk_with_TAG.mp3":          "rejected",
		"128KiB_plus_one_junk_with_TAG.mp3": "rejected",
	}
	for file, want := range cases {
		t.Run(file, func(t *testing.T) {
			rep, err := VerifyMP3(filepath.Join(dir, file), VerifyExpectation{MaxBitrateKbps: 320})
			got := "verified"
			if err != nil {
				got = "rejected"
			} else if rep.ValidationLevel == "degraded" {
				got = "degraded"
			}
			if got != want {
				t.Errorf("%s: 期望 %s，got %s (err=%v)", file, want, got, err)
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
