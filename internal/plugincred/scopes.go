// Package plugincred 定义插件凭据的 HTTP scope 语义。
//
// 插件凭据（plugin JWT）按 manifest 权限签发 scope；认证中间件读到
// Kind=plugin 时，按「方法 + 路径」判定所需 scope，未声明或无法归类的
// 请求一律拒绝（fail closed）。管理面、鉴权面与插件管理端点没有对应
// scope，因此插件票据永远无法触达。
//
// 本包刻意不依赖 jsplugin，避免 services → jsplugin → services 的循环；
// scope 字面量与 jsplugin 的权限常量由测试锁定一致。
package plugincred

import (
	"strings"
	"time"
)

// Mode 是插件凭据的 scope 策略档位。
type Mode string

const (
	// ModeAuto 窗口内 legacy、到期 strict。
	ModeAuto Mode = "auto"
	// ModeLegacy 始终使用全插件 scope（兼容档）。
	ModeLegacy Mode = "legacy"
	// ModeStrict 始终使用 manifest 最小 scope。
	ModeStrict Mode = "strict"
)

// Valid 报告档位是否合法。
func (m Mode) Valid() bool {
	return m == ModeAuto || m == ModeLegacy || m == ModeStrict
}

// EffectiveMode 返回当前生效的 scope 档位。
func EffectiveMode(mode Mode, legacyUntil string, now time.Time) Mode {
	switch mode {
	case ModeLegacy:
		return ModeLegacy
	case ModeStrict:
		return ModeStrict
	}
	if LegacyWindowOpen(mode, legacyUntil, now) {
		return ModeLegacy
	}
	return ModeStrict
}

// LegacyWindowOpen 报告旧插件票据窗口是否仍开放。
// strict 档显式关闭；缺失/非法截止时间按已结束处理（fail closed）。
func LegacyWindowOpen(mode Mode, legacyUntil string, now time.Time) bool {
	if mode == ModeStrict {
		return false
	}
	until, err := time.Parse(time.RFC3339, legacyUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// LegacyUntilConfigKey 是旧插件票据（百年 plugin-system token）的兼容窗口
// 截止时间配置键（RFC3339）。窗口结束即拒绝；缺失或非法按已结束处理。
const LegacyUntilConfigKey = "plugin_credential_legacy_until"

// ModeConfigKey 是插件凭据模式配置键（auto/legacy/strict）。
// strict 档关闭旧票据窗口，即使截止时间尚未到。
const ModeConfigKey = "plugin_credential_mode"

// HTTP 插件 scope。取值与 jsplugin 权限声明一致（manifest 的 permissions）。
const (
	ScopeSongsRead      = "songs.read"
	ScopeSongsWrite     = "songs.write"
	ScopePlaylistsRead  = "playlists.read"
	ScopePlaylistsWrite = "playlists.write"
	ScopeTagsRead       = "tags.read"
	ScopeTagsWrite      = "tags.write"
	ScopeNet            = "net"
)

// HTTPPluginScopes 是插件票据能承载的全部 HTTP scope（legacy 兼容档使用）。
var HTTPPluginScopes = []string{
	ScopeSongsRead, ScopeSongsWrite,
	ScopePlaylistsRead, ScopePlaylistsWrite,
	ScopeTagsRead, ScopeTagsWrite,
	ScopeNet,
}

// HasScope 判断 scope 集合是否包含指定 scope（精确匹配，不做前缀糖——
// 通配符在签发侧已展开为具体 scope）。
func HasScope(scopes []string, required string) bool {
	for _, s := range scopes {
		if s == required {
			return true
		}
	}
	return false
}

// PluginSelfEntry 解析 /api/v1/jsplugin/{entryPath}/... 形式的插件自身路径。
// 返回 entry 与 ok；管理面 /api/v1/jsplugins/... 不匹配（多一个 s）。
func PluginSelfEntry(path string) (string, bool) {
	const prefix = "/api/v1/jsplugin/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	entry, _, _ := strings.Cut(rest, "/")
	if entry == "" {
		return "", false
	}
	return entry, true
}

// RequiredScope 返回访问指定方法与路径所需的插件 scope。
// ok=false 表示该路径不在插件可访问面内，必须拒绝。
func RequiredScope(method, path string) (string, bool) {
	// 管理面与插件管理端点没有对应 scope。
	if strings.HasPrefix(path, "/api/v1/jsplugins") || strings.HasPrefix(path, "/api/v1/plugins/") {
		return "", false
	}

	read := method == "GET" || method == "HEAD"

	// 标签路径要先于歌曲判定：/songs/{id}/song-tags 属于标签域。
	if strings.HasPrefix(path, "/api/v1/song-tags") || strings.Contains(path, "/song-tags") {
		if read {
			return ScopeTagsRead, true
		}
		return ScopeTagsWrite, true
	}

	if strings.HasPrefix(path, "/api/v1/play-history") {
		if read {
			return ScopeSongsRead, true
		}
		return "", false
	}

	if strings.HasPrefix(path, "/api/v1/songs") {
		if read {
			return ScopeSongsRead, true
		}
		return ScopeSongsWrite, true
	}

	if strings.HasPrefix(path, "/api/v1/playlists") {
		if read {
			return ScopePlaylistsRead, true
		}
		return ScopePlaylistsWrite, true
	}

	if strings.HasPrefix(path, "/api/v1/proxy") {
		return ScopeNet, true
	}

	return "", false
}
