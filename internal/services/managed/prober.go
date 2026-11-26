package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultProber 返回一个基于 ffprobe 的 Prober；PATH 中找不到 ffprobe 时返回 nil
// （验证器按「跳过交叉验证」处理，原因记入 ValidationReason）。
// 生产 expectation 用它启用独立解码交叉验证（ticket 01 / F10 分层验证第 3 层）。
func DefaultProber() Prober {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil
	}
	return func(path string) (codec string, durationSec float64, err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin,
			"-v", "error",
			"-show_entries", "stream=codec_name:format=duration",
			"-of", "json", path,
		).Output()
		if err != nil {
			return "", 0, fmt.Errorf("ffprobe %s: %w", path, err)
		}
		var doc struct {
			Streams []struct {
				CodecName string `json:"codec_name"`
			} `json:"streams"`
			Format struct {
				Duration string `json:"duration"`
			} `json:"format"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			return "", 0, fmt.Errorf("ffprobe json: %w", err)
		}
		if len(doc.Streams) == 0 {
			return "", 0, fmt.Errorf("ffprobe: no streams")
		}
		codec = strings.ToLower(strings.TrimSpace(doc.Streams[0].CodecName))
		if doc.Format.Duration != "" && doc.Format.Duration != "N/A" {
			_, _ = fmt.Sscanf(doc.Format.Duration, "%f", &durationSec)
		}
		return codec, durationSec, nil
	}
}
