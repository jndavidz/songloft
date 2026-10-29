package jsplugin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"songloft/internal/models"
)

// 类型别名：JSPlugin 和 JSPluginStatus 定义在 models 包中，此处为向后兼容保留别名
type JSPlugin = models.JSPlugin
type JSPluginStatus = models.JSPluginStatus

// 状态常量别名
const (
	JSPluginStatusActive   = models.JSPluginStatusActive
	JSPluginStatusInactive = models.JSPluginStatusInactive
	JSPluginStatusError    = models.JSPluginStatusError
)

// PluginManifest 对应 plugin.json 文件结构。
//
// 开发期严格校验：`entryHash` / `zipHash` 为必填字段，由
// @songloft/plugin-builder 打包时自动写入，后端在 InstallFromUpload /
// Update / Load 中都会校验实际内容与字段值严格一致。
type PluginManifest struct {
	Schema         string `json:"$schema,omitempty"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Description    string `json:"description"`
	Author         string `json:"author"`
	Homepage       string `json:"homepage,omitempty"`
	License        string `json:"license,omitempty"`
	EntryPath      string `json:"entryPath"`
	Main           string `json:"main"`
	MinHostVersion string `json:"minHostVersion,omitempty"`
	// Capabilities 声明插件需要宿主提供的执行能力（可选）。
	// 宿主能力面见 HostPluginCapabilities；声明了宿主不支持的能力时，
	// 安装/更新/加载一律失败关闭（S-02）。
	Capabilities  []string `json:"capabilities,omitempty"`
	Permissions   []string `json:"permissions"`
	PublicPaths   []string `json:"publicPaths,omitempty"`
	ExternalPaths []string `json:"externalPaths,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	UpdateURL     string   `json:"updateUrl,omitempty"`
	DownloadURL   string   `json:"download_url,omitempty"`
	// EntryHash 为入口文件（main.js 或 main.jsc）的 sha256，64 位小写 hex，必填。
	EntryHash string `json:"entryHash"`
	// ZipHash 为 zip 内除 plugin.json 外所有文件的规范化 sha256，必填。
	ZipHash string `json:"zipHash"`
	// RenderEngine 声明插件页面由哪个引擎渲染（songloft-org/songloft#341）。
	// 可选，取值只允许 RenderEngineWebView / RenderEngineLynx；
	// 缺失或空串表示「跟随宿主默认」（当前默认系统 WebView）。
	RenderEngine string `json:"renderEngine,omitempty"`
}

// 插件页渲染引擎取值（plugin.json 的 renderEngine 字段 / DB 的 render_engine 列）。
// 空串同样合法，语义为「跟随宿主默认」，故刻意不定义 RenderEngineDefault 常量以免
// 被误当成需要落库的具体值。
const (
	// RenderEngineWebView 使用客户端系统 WebView 渲染（宿主默认）。
	RenderEngineWebView = "webview"
	// RenderEngineLynx 使用 Lynx 原生渲染（插件以 .lynx.bundle 分发）。
	RenderEngineLynx = "lynx"
)

// IsValidRenderEngine 判断 renderEngine 取值是否合法。空串合法（= 跟随宿主默认）。
func IsValidRenderEngine(v string) bool {
	switch v {
	case "", RenderEngineWebView, RenderEngineLynx:
		return true
	default:
		return false
	}
}

var (
	entryPathRegexp = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	semverRegexp    = regexp.MustCompile(`^\d+\.\d+\.\d+`)
)

// ParseManifest 从 JSON 字节解析 plugin.json
func ParseManifest(data []byte) (*PluginManifest, error) {
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse plugin.json: %w", err)
	}
	return &m, nil
}

// ValidateManifest 验证必填字段
func ValidateManifest(m *PluginManifest) error {
	// name: 2-50 字符，必填
	if len(m.Name) < 2 || len(m.Name) > 50 {
		return fmt.Errorf("name must be 2-50 characters, got %d", len(m.Name))
	}

	// version: semver 格式，必填
	if m.Version == "" {
		return fmt.Errorf("version is required")
	}
	if !semverRegexp.MatchString(m.Version) {
		return fmt.Errorf("version must be semver format (e.g. 1.0.0), got %q", m.Version)
	}

	// entryPath: 小写字母+数字+连字符，必填
	if m.EntryPath == "" {
		return fmt.Errorf("entryPath is required")
	}
	if !entryPathRegexp.MatchString(m.EntryPath) {
		return fmt.Errorf("entryPath must match ^[a-z][a-z0-9-]*$, got %q", m.EntryPath)
	}

	// main: 必填，必须以 .js 或 .jsc 结尾
	if m.Main == "" {
		return fmt.Errorf("main is required")
	}
	if !strings.HasSuffix(m.Main, ".js") && !strings.HasSuffix(m.Main, ".jsc") {
		return fmt.Errorf("main must end with .js or .jsc, got %q", m.Main)
	}

	// permissions: 必填（可为空数组）
	if m.Permissions == nil {
		return fmt.Errorf("permissions is required (can be empty array)")
	}

	// renderEngine: 可选，只允许 "" / "webview" / "lynx"
	if !IsValidRenderEngine(m.RenderEngine) {
		return fmt.Errorf("renderEngine must be %q or %q (or omitted), got %q",
			RenderEngineWebView, RenderEngineLynx, m.RenderEngine)
	}

	// entryHash / zipHash: 必填，64 位小写 hex。
	if err := ValidateHashField("entryHash", m.EntryHash); err != nil {
		return err
	}
	if err := ValidateHashField("zipHash", m.ZipHash); err != nil {
		return err
	}

	return nil
}

// EntryPathFromZipName 从 ZIP 文件名提取 entryPath
// 例如: "myplugin.jsplugin.zip" -> "myplugin"
func EntryPathFromZipName(zipName string) string {
	name := zipName
	// 移除 .zip 后缀
	name = strings.TrimSuffix(name, ".zip")
	// 移除 .jsplugin 后缀
	name = strings.TrimSuffix(name, ".jsplugin")
	return name
}
