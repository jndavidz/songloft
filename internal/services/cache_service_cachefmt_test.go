package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"songloft/internal/models"
)

// newTestCacheService 构造仅设置并发安全 atomic 字段的 CacheService，供测试使用。
func newTestCacheService(cacheDir, ffmpegPath, transcodeFormat string) *CacheService {
	cs := &CacheService{}
	if cacheDir != "" {
		cs.cacheDir.Store(cacheDir)
	}
	if ffmpegPath != "" {
		cs.ffmpegPath.Store(ffmpegPath)
	}
	if transcodeFormat != "" {
		cs.cacheTranscodeFormat.Store(transcodeFormat)
	}
	return cs
}

func TestNormalizeTranscodeFormat(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"mp3", "mp3"},
		{"MP3", "mp3"},
		{".mp3", "mp3"},
		{"m4a", "m4a"},
		{"aac", "m4a"},
		{"mp4", "m4a"},
		{"ogg", "ogg"},
		{"vorbis", "ogg"},
		{"flac", "flac"},
		{"wav", "wav"},
		{"", ""},     // 空=不转码
		{"mkv", ""},  // 视频容器不允许
		{"webm", ""}, // 视频容器不允许
		{"wma", ""},  // ffmpegArgs 不支持
		{"ape", ""},  // ffmpegArgs 不支持
		{"xyz", ""},  // 未知
	}
	for _, tt := range tests {
		if got := NormalizeTranscodeFormat(tt.input); got != tt.want {
			t.Errorf("NormalizeTranscodeFormat(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// writeCacheFile 在临时目录写一个假的缓存文件，返回其路径。
func writeCacheFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("fake audio bytes"), 0644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}
	return p
}

func TestEnsureCachedFormat_Noop(t *testing.T) {
	song := &models.Song{ID: 7, Type: "remote", URL: "http://example.com/a.mkv"}

	t.Run("format not configured", func(t *testing.T) {
		dir := t.TempDir()
		cs := newTestCacheService(dir, "/bin/echo", "") // ffmpeg 有，但未配置格式
		src := writeCacheFile(t, dir, "src.mkv")
		got := cs.EnsureCachedFormat(context.Background(), song, src)
		if got != src {
			t.Errorf("got %q, want unchanged %q", got, src)
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("original file should remain: %v", err)
		}
	})

	t.Run("ffmpeg unavailable", func(t *testing.T) {
		dir := t.TempDir()
		cs := newTestCacheService(dir, "", "mp3") // 配了格式但无 ffmpeg
		src := writeCacheFile(t, dir, "src.mkv")
		got := cs.EnsureCachedFormat(context.Background(), song, src)
		if got != src {
			t.Errorf("got %q, want unchanged %q", got, src)
		}
	})

	t.Run("already target format", func(t *testing.T) {
		dir := t.TempDir()
		cs := newTestCacheService(dir, "/bin/echo", "mp3")
		// 复制真实 MP3 样本，让 tag.ReadFrom 能识别为 MP3
		src := filepath.Join(dir, "src.mp3")
		data, err := os.ReadFile("../../pkg/tag/testdata/with_tags/sample.id3v23.mp3")
		if err != nil {
			t.Fatalf("read sample mp3: %v", err)
		}
		if err := os.WriteFile(src, data, 0644); err != nil {
			t.Fatalf("write cache file: %v", err)
		}
		got := cs.EnsureCachedFormat(context.Background(), song, src)
		if got != src {
			t.Errorf("got %q, want unchanged %q (same format short-circuit)", got, src)
		}
	})

	t.Run("nil song / empty path", func(t *testing.T) {
		cs := newTestCacheService("", "/bin/echo", "mp3")
		if got := cs.EnsureCachedFormat(context.Background(), nil, "/x.mkv"); got != "/x.mkv" {
			t.Errorf("nil song: got %q, want unchanged", got)
		}
		if got := cs.EnsureCachedFormat(context.Background(), song, ""); got != "" {
			t.Errorf("empty path: got %q, want empty", got)
		}
	})
}

func TestEnsureCachedFormat_DegradeOnFailure(t *testing.T) {
	dir := t.TempDir()
	// ffmpeg 路径指向不存在的可执行文件 → runFFmpeg 启动失败 → 优雅降级保留原码
	cs := newTestCacheService(dir, filepath.Join(dir, "nonexistent-ffmpeg"), "mp3")
	song := &models.Song{ID: 9, Type: "remote", URL: "http://example.com/b.mkv"}
	src := writeCacheFile(t, dir, "src.mkv")

	got := cs.EnsureCachedFormat(context.Background(), song, src)
	if got != src {
		t.Errorf("on ffmpeg failure got %q, want original %q", got, src)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("original file should be kept on failure: %v", err)
	}
	// 不应残留临时转码文件
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cachetc-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
