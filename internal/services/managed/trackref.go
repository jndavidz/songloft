// Package managed 实现 Songloft 侧托管播放的当前项准备域：TrackRef 身份契约、
// kwqq v2 解析客户端、最终表示验证与准备操作状态机（S-03）。
//
// 契约事实源：musicdl 仓库 server/trackref.py（跨项目唯一主 spec 的对偶实现）。
// 本包的 Canonical/Digest 必须与 Python 侧逐字节一致（golden 向量锁定）。
package managed

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Provider 是 TrackRef 的固定 provider 标识（契约与 musicdl trackref.py 一致）。
const Provider = "kwqq"

// Ref 是稳定曲目身份（TrackRef）：source + id + 仅影响身份的 extra 字段。
// extra 白名单外的字段一律丢弃，缓存键不能被非身份噪声稀释。
type Ref struct {
	Kind     string         `json:"kind"`
	Provider string         `json:"provider"`
	Source   string         `json:"source"`
	ID       string         `json:"id"`
	Extra    map[string]any `json:"extra"`
}

// RefError 对应 Python 侧 trackref.RefError：畸形/不完整身份是调用方错误，
// 绝不按「尽力而为」解析。
type RefError struct{ msg string }

func (e *RefError) Error() string { return e.msg }

func refErr(format string, args ...any) error {
	return &RefError{msg: fmt.Sprintf(format, args...)}
}

// identityFields 是各 source 影响身份的 extra 白名单（契约：IDENTITY_FIELDS）。
var identityFields = map[string][]string{
	"migu":     {"copyrightId"},
	"bilibili": {"cid", "page"},
}

// requiredExtra 是各 source 至少需要其一的 extra 字段（契约：REQUIRED_EXTRA）。
var requiredExtra = map[string][]string{
	"migu":     {"copyrightId"},
	"bilibili": {"cid", "page"},
}

// MakeRef 构造并校验 TrackRef。sources 非空时同时校验 source 注册身份。
// extra 中的值按契约类型化：bilibili page 保持 int（canonical 里是 JSON number），
// cid 规范化为正数字字符串。
func MakeRef(source, id string, extra map[string]any, sources []string) (*Ref, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, refErr("track_ref.source is required")
	}
	if len(sources) > 0 {
		known := false
		for _, s := range sources {
			if s == source {
				known = true
				break
			}
		}
		if !known {
			return nil, refErr("unknown source %q (available: %s)", source, strings.Join(sources, ", "))
		}
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, refErr("track_ref.id is required")
	}

	norm, err := normalizeExtra(source, extra)
	if err != nil {
		return nil, err
	}
	for _, f := range requiredExtra[source] {
		if _, ok := norm[f]; ok {
			return &Ref{Kind: "remote", Provider: Provider, Source: source, ID: id, Extra: norm}, nil
		}
	}
	if req := requiredExtra[source]; len(req) > 0 {
		if len(req) == 1 {
			return nil, refErr("%s track_ref requires extra.%s", source, req[0])
		}
		return nil, refErr("%s track_ref requires extra.%s", source, strings.Join(req, " or extra."))
	}
	return &Ref{Kind: "remote", Provider: Provider, Source: source, ID: id, Extra: norm}, nil
}

func normalizeExtra(source string, extra map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, field := range identityFields[source] {
		v, ok := extra[field]
		if !ok || v == nil {
			continue
		}
		switch field {
		case "cid":
			s := strings.TrimSpace(toStr(v))
			if !isAllDigits(s) {
				return nil, refErr("bilibili extra.cid must be a positive numeric cid, got %s", pythonRepr(v))
			}
			if n, err := strconv.Atoi(s); err != nil || n <= 0 {
				return nil, refErr("bilibili extra.cid must be a positive numeric cid, got %s", pythonRepr(v))
			}
			out["cid"] = s
		case "page":
			n, ok := toInt(v)
			if !ok {
				return nil, refErr("bilibili extra.page must be an integer page number, got %s", pythonRepr(v))
			}
			if n < 1 {
				return nil, refErr("bilibili extra.page must be >= 1, got %d", n)
			}
			out["page"] = n
		default:
			out[field] = strings.TrimSpace(toStr(v))
		}
	}
	return out, nil
}

// Canonical 是唯一结构化规范编码（缓存/去重键材）：键排序、无空白、extra 内
// 键序不影响结果。与 Python json.dumps(sort_keys=True, separators=(',',':'),
// ensure_ascii=False) 逐字节一致（golden 向量锁定）。
func (r *Ref) Canonical() (string, error) {
	if r == nil {
		return "", refErr("nil ref")
	}
	extra := map[string]any{}
	for k, v := range r.Extra {
		extra[k] = v
	}
	payload := map[string]any{
		"provider": orDefault(r.Provider, Provider),
		"kind":     orDefault(r.Kind, "remote"),
		"source":   r.Source,
		"id":       r.ID,
		"extra":    extra,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return "", err
	}
	// json.Encoder 追加换行；Python json.dumps 无换行。
	return strings.TrimRight(buf.String(), "\n"), nil
}

// Digest 是 canonical 的 SHA-256 前 32 个十六进制字符（契约一致）。
func (r *Ref) Digest() (string, error) {
	c, err := r.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(c))
	return fmt.Sprintf("%x", sum[:16]), nil
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON 解码路径里的数字
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
		return 0, false
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pythonRepr 让错误信息与 Python 侧 %r 输出同形（便于跨栈对账）。
func pythonRepr(v any) string {
	switch t := v.(type) {
	case string:
		return "'" + t + "'"
	case nil:
		return "None"
	default:
		return toStr(t)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
