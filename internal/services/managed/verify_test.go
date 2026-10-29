package managed

import (
	"path/filepath"
	"strings"
	"testing"
)

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

// TestVerifyMP3_Valid：真实 MP3（带 ID3v2 前缀）通过完整验证并如实报告帧事实。
func TestVerifyMP3_Valid(t *testing.T) {
	for _, name := range []string{"valid_128k.mp3", "valid_320k.mp3"} {
		t.Run(name, func(t *testing.T) {
			rep, err := VerifyMP3(fixturePath(t, name), VerifyExpectation{MaxBitrateKbps: 320})
			if err != nil {
				t.Fatalf("%s 应通过验证: %v", name, err)
			}
			if rep.FrameCount == 0 {
				t.Error("帧数不应为 0")
			}
			if rep.DurationEstSec < 1.0 || rep.DurationEstSec > 2.0 {
				t.Errorf("时长估计应约 1.5s，got %.3f", rep.DurationEstSec)
			}
			if rep.ID3PrefixBytes == 0 && name == "valid_128k.mp3" {
				t.Error("valid_128k.mp3 应有 ID3v2 前缀")
			}
		})
	}
}

// TestVerifyMP3_320kAtCeiling：320k 正好在托管契约上限内，不误杀。
func TestVerifyMP3_320kAtCeiling(t *testing.T) {
	rep, err := VerifyMP3(fixturePath(t, "valid_320k.mp3"), VerifyExpectation{MaxBitrateKbps: 320})
	if err != nil {
		t.Fatalf("320k 应在 320 上限内通过: %v", err)
	}
	if rep.MaxFrameKbps != 320 {
		t.Errorf("MaxFrameKbps 应为 320，got %d", rep.MaxFrameKbps)
	}
}

func TestVerifyMP3_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		wantSub string
		size    int64
	}{
		{"FLAC 冒充 mp3", "flac_as_mp3.mp3", "", 0},
		{"AAC 冒充 mp3", "aac_as_mp3.mp3", "", 0},
		{"纯文本冒充 mp3", "not_audio.mp3", "", 0},
		{"截断帧流", "truncated.mp3", "", 0},
		{"声明大小不符", "valid_128k.mp3", "size mismatch", 12345},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyMP3(fixturePath(t, tc.file), VerifyExpectation{DeclaredSize: tc.size})
			if err == nil {
				t.Fatalf("%s 应被拒绝", tc.file)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误应含 %q，got %q", tc.wantSub, err.Error())
			}
		})
	}
}

// TestVerifyMP3_ProberCrossCheck：注入 prober 时 codec/时长交叉验证生效。
func TestVerifyMP3_ProberCrossCheck(t *testing.T) {
	t.Run("codec 非 mp3 拒绝", func(t *testing.T) {
		_, err := VerifyMP3(fixturePath(t, "valid_128k.mp3"), VerifyExpectation{
			Prober: func(path string) (string, float64, error) { return "flac", 1.5, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "not mp3") {
			t.Errorf("应拒绝非 mp3 codec，got %v", err)
		}
	})
	t.Run("时长分歧超容差拒绝", func(t *testing.T) {
		_, err := VerifyMP3(fixturePath(t, "valid_128k.mp3"), VerifyExpectation{
			Prober: func(path string) (string, float64, error) { return "mp3", 120.0, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "diverges") {
			t.Errorf("应拒绝时长分歧，got %v", err)
		}
	})
	t.Run("一致时通过", func(t *testing.T) {
		rep, err := VerifyMP3(fixturePath(t, "valid_128k.mp3"), VerifyExpectation{
			Prober: func(path string) (string, float64, error) { return "mp3", 1.5, nil },
		})
		if err != nil {
			t.Fatalf("一致事实应通过: %v", err)
		}
		if rep.ProbeCodec != "mp3" {
			t.Errorf("报告应记录 probe codec，got %q", rep.ProbeCodec)
		}
	})
}
