package managed

import (
	"testing"
)

// golden 向量由 musicdl server/trackref.py（契约事实源）直接运行产出，
// 锁定 Go 侧 Canonical/Digest 与 Python 逐字节一致。
func TestRef_GoldenVectors(t *testing.T) {
	cases := []struct {
		name      string
		source    string
		id        string
		extra     map[string]any
		canonical string
		digest    string
	}{
		{
			// kwqq 源无身份 extra 字段：source_data 里的 c（copyrightId 载体）按契约丢弃，
			// migu 源才保留 copyrightId。向量 = Python make_ref 产出。
			name:   "kwqq 非身份 extra 字段丢弃",
			source: "kwqq", id: "12345", extra: map[string]any{"c": "445566"},
			canonical: `{"extra":{},"id":"12345","kind":"remote","provider":"kwqq","source":"kwqq"}`,
			digest:    "f1c97fcd7a19befd3a536cdffd860acc",
		},
		{
			name:   "migu copyrightId 保留",
			source: "migu", id: "m01", extra: map[string]any{"copyrightId": "6900001"},
			canonical: `{"extra":{"copyrightId":"6900001"},"id":"m01","kind":"remote","provider":"kwqq","source":"migu"}`,
			digest:    "b5de8cafe464439dc850cf10fbaa2df9",
		},
		{
			name:   "bilibili cid 规范化为字符串 page 保持 number",
			source: "bilibili", id: "BV1x", extra: map[string]any{"cid": 30251, "page": 2},
			canonical: `{"extra":{"cid":"30251","page":2},"id":"BV1x","kind":"remote","provider":"kwqq","source":"bilibili"}`,
			digest:    "0796c05058a67c16d7363793f983b8b2",
		},
		{
			name:   "bilibili 仅 page",
			source: "bilibili", id: "BV1y", extra: map[string]any{"page": 1},
			canonical: `{"extra":{"page":1},"id":"BV1y","kind":"remote","provider":"kwqq","source":"bilibili"}`,
			digest:    "342aad946b9bdc1f6a9b40b3d6b614ec",
		},
		{
			name:   "kwqq 空 extra",
			source: "kwqq", id: "abc", extra: map[string]any{},
			canonical: `{"extra":{},"id":"abc","kind":"remote","provider":"kwqq","source":"kwqq"}`,
			digest:    "1496f82c136e5f27cb285f64e968e869",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := MakeRef(tc.source, tc.id, tc.extra, nil)
			if err != nil {
				t.Fatalf("MakeRef: %v", err)
			}
			got, err := ref.Canonical()
			if err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if got != tc.canonical {
				t.Errorf("canonical mismatch:\n got  %s\n want %s", got, tc.canonical)
			}
			d, err := ref.Digest()
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if d != tc.digest {
				t.Errorf("digest mismatch: got %s want %s", d, tc.digest)
			}
		})
	}
}

// TestRef_ExtraKeyOrderIrrelevant：extra 键序不影响 canonical（缓存键不能被键序稀释）。
func TestRef_ExtraKeyOrderIrrelevant(t *testing.T) {
	a, _ := MakeRef("bilibili", "BV1x", map[string]any{"cid": "30251", "page": 2}, nil)
	b, _ := MakeRef("bilibili", "BV1x", map[string]any{"page": 2, "cid": "30251"}, nil)
	ca, _ := a.Canonical()
	cb, _ := b.Canonical()
	if ca != cb {
		t.Errorf("extra 键序应不影响 canonical:\n%s\n%s", ca, cb)
	}
}

// TestRef_Rejections：缺字段/畸形 extra 明确拒绝，不按尽力而为解析。
func TestRef_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		source string
		id     string
		extra  map[string]any
	}{
		{"migu 缺 copyrightId", "migu", "m02", map[string]any{}},
		{"bilibili 缺 cid 和 page", "bilibili", "BV1z", map[string]any{}},
		{"bilibili cid 非数字", "bilibili", "BV1z", map[string]any{"cid": "abc"}},
		{"bilibili cid 非正数", "bilibili", "BV1z", map[string]any{"cid": "0"}},
		{"bilibili page < 1", "bilibili", "BV1z", map[string]any{"page": 0}},
		{"bilibili page 非整数", "bilibili", "BV1z", map[string]any{"page": "x"}},
		{"空 source", "", "x", nil},
		{"空 id", "kwqq", "  ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MakeRef(tc.source, tc.id, tc.extra, nil); err == nil {
				t.Errorf("应拒绝 (%s, %s, %v)", tc.source, tc.id, tc.extra)
			}
		})
	}
}

// TestRef_UnknownSource：注册表校验拒绝未知 source。
func TestRef_UnknownSource(t *testing.T) {
	if _, err := MakeRef("youtube", "x", nil, []string{"kwqq", "migu", "bilibili"}); err == nil {
		t.Error("未知 source 应被拒绝")
	}
	if _, err := MakeRef("migu", "m1", map[string]any{"copyrightId": "1"},
		[]string{"kwqq", "migu", "bilibili"}); err != nil {
		t.Errorf("已注册 source 不应被拒绝: %v", err)
	}
}

// TestRef_DigestStable：同身份重复 digest 稳定；异身份 digest 不同（同 BV 不同分 P 不串）。
func TestRef_DigestStable(t *testing.T) {
	p1, _ := MakeRef("bilibili", "BV1x", map[string]any{"page": 1}, nil)
	p2, _ := MakeRef("bilibili", "BV1x", map[string]any{"page": 2}, nil)
	d1, _ := p1.Digest()
	d2, _ := p2.Digest()
	if d1 == d2 {
		t.Error("同 BV 不同 page 必须不同 digest（防串缓存/串播放）")
	}
	m1, _ := MakeRef("migu", "m01", map[string]any{"copyrightId": "6900001"}, nil)
	m2, _ := MakeRef("migu", "m01", map[string]any{"copyrightId": "6900002"}, nil)
	dm1, _ := m1.Digest()
	dm2, _ := m2.Digest()
	if dm1 == dm2 {
		t.Error("同 migu id 不同 copyrightId 必须不同 digest")
	}
}
