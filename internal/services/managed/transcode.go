package managed

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// 真实转码器：AAC 规范化输入 → MP3（B站 AAC→MP3 是 transcode 不是 remux，
// 契约 5.1 规则 9）。输入只接受准备链拉取的中间表示；输出供 VerifyMP3 终验。

// TranscoderLimits 约束 ffmpeg 转码（S-05 会并入全局预算，首版本包有界即可）。
type TranscoderLimits struct {
	FfmpegPath string // 空 = PATH 中的 ffmpeg
	Timeout    time.Duration
	// Sem 限制并发转码的通道；nil = 包级单进程串行（与现有转码语义一致，防 CPU 风暴）。
	Sem chan struct{}
}

var transcodeSem = make(chan struct{}, 1)

// TranscodeToMP3 把 input 转码为 destDir/intermediate.mp3。
// 只产出中间命名——只有 VerifyMP3 通过后才允许以 ready/最终名投送。
func TranscodeToMP3(ctx context.Context, input, destDir string, limits TranscoderLimits) (string, error) {
	ff := limits.FfmpegPath
	if ff == "" {
		ff = "ffmpeg"
	}
	timeout := limits.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if limits.Sem != nil {
		select {
		case limits.Sem <- struct{}{}:
			defer func() { <-limits.Sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else {
		transcodeSem <- struct{}{}
		defer func() { <-transcodeSem }()
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	outPath := filepath.Join(destDir, "intermediate.mp3")
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ff,
		"-y", "-i", input,
		"-codec:a", "libmp3lame", "-b:a", "192k",
		"-f", "mp3", outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(outPath)
		return "", fmt.Errorf("transcode to mp3: %w: %s", err, tailBytes(out, 512))
	}
	return outPath, nil
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
