package jsplugin

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"songloft/internal/version"
)

// 插件执行兼容门（S-02）
//
// 安装、上传、更新、自动更新、启动加载与懒加载都必须在真正执行前判定
// 插件声明的 minHostVersion 与 capabilities 是否被当前宿主满足；不满足
// 一律失败关闭。UI 提示不是安全边界，安装成功也不等于允许运行。

// HostPluginCapabilities 是当前宿主能提供的插件能力面。
//
// 取的是插件系统实际支持的能力集合（音乐源、歌词/封面提供者、静态页面、
// 命令与插件间通信）。manifest 声明了未知能力说明它是为别的宿主写的，
// 在能力面前失败关闭，而不是带着未知假设运行。
var HostPluginCapabilities = []string{
	"music-source",
	"lyrics-provider",
	"cover-provider",
	"page",
	"command",
	"inter-plugin",
}

var (
	hostVersionFieldRegexp = regexp.MustCompile(`^v?\d+(\.\d+){0,2}$`)
	// normalizeVersionRegexp 从构建注入的版本里取前三段数字。
	// 发布构建常用 `git describe`（v2.13.6-3-gabc123）或 v 前缀，
	// 直接喂给 CompareVersion 会把整段当数字解析成 0。
	normalizeVersionRegexp = regexp.MustCompile(`^[vV]?(\d+)(?:\.(\d+))?(?:\.(\d+))?`)
)

// HostVersion 返回用于兼容判定的宿主版本。
func HostVersion() string {
	return version.Version
}

// ValidateHostVersionField 校验 minHostVersion 的字面格式。
// 空值合法（不声明要求）。
func ValidateHostVersionField(minHostVersion string) error {
	if minHostVersion == "" {
		return nil
	}
	if !hostVersionFieldRegexp.MatchString(minHostVersion) {
		return fmt.Errorf("minHostVersion must look like 1.2.3, got %q", minHostVersion)
	}
	return nil
}

// normalizeVersion 把 v 前缀 / git describe 后缀归一成 x.y.z。
func normalizeVersion(raw string) (string, bool) {
	match := normalizeVersionRegexp.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return "", false
	}
	parts := []string{match[1]}
	if match[2] != "" {
		parts = append(parts, match[2])
	}
	if match[3] != "" {
		parts = append(parts, match[3])
	}
	return strings.Join(parts, "."), true
}

// checkMinHostVersion 校验宿主版本是否满足要求。
//
// 宿主版本为 dev（未注入）时跳过比较：开发构建不应被 minHostVersion 卡住。
// 其它无法解析的宿主版本失败关闭，避免带着未知版本放行。
func checkMinHostVersion(minHostVersion string) error {
	if minHostVersion == "" {
		return nil
	}
	if err := ValidateHostVersionField(minHostVersion); err != nil {
		return err
	}
	hostVersion := HostVersion()
	if hostVersion == "dev" {
		return nil
	}
	normalizedHost, ok := normalizeVersion(hostVersion)
	if !ok {
		return fmt.Errorf("宿主版本 %q 无法解析，无法判定插件兼容性", hostVersion)
	}
	normalizedMin, ok := normalizeVersion(minHostVersion)
	if !ok {
		return fmt.Errorf("minHostVersion %q 无法解析", minHostVersion)
	}
	if CompareVersion(normalizedHost, normalizedMin) < 0 {
		return fmt.Errorf("插件要求宿主版本 >= %s，当前 %s", minHostVersion, hostVersion)
	}
	return nil
}

// CheckManifestCompatibility 判定 manifest 是否可在当前宿主执行。
func CheckManifestCompatibility(manifest *PluginManifest) error {
	if manifest == nil {
		return fmt.Errorf("manifest is nil")
	}
	if err := checkMinHostVersion(manifest.MinHostVersion); err != nil {
		return err
	}
	return validateCapabilities(manifest.Capabilities)
}

func validateCapabilities(capabilities []string) error {
	if len(capabilities) == 0 {
		return nil
	}
	supported := make(map[string]struct{}, len(HostPluginCapabilities))
	for _, capability := range HostPluginCapabilities {
		supported[capability] = struct{}{}
	}
	for _, capability := range capabilities {
		if capability == "" {
			return fmt.Errorf("capabilities 含空值")
		}
		if _, ok := supported[capability]; !ok {
			return fmt.Errorf("宿主不支持插件能力 %q", capability)
		}
	}
	return nil
}

// CheckPluginRecordCompatibility 在加载边界校验已落库的插件记录。
// capabilities 由调用方从 zip 内 manifest 读取（DB 不存能力声明）。
func CheckPluginRecordCompatibility(plugin *JSPlugin, capabilities []string) error {
	if err := checkMinHostVersion(plugin.MinHostVersion); err != nil {
		return err
	}
	return validateCapabilities(capabilities)
}

// readCapabilitiesFromPluginZip 从插件 ZIP 的 plugin.json 读取能力声明。
// 读取/解析失败返回错误：加载门必须失败关闭，不能把「读不到」当成「无要求」。
func readCapabilitiesFromPluginZip(zipPath string) ([]string, error) {
	if zipPath == "" {
		return nil, fmt.Errorf("插件 ZIP 路径缺失")
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return nil, fmt.Errorf("读取插件 ZIP 失败: %w", err)
	}
	manifest, err := readPluginManifestFromZip(data)
	if err != nil {
		return nil, fmt.Errorf("解析插件 manifest 失败: %w", err)
	}
	return manifest.Capabilities, nil
}
