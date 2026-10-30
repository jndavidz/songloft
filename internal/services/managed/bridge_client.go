package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BridgeClient 是 BFF→bridge 的命令客户端（B-08 缺口的 Songloft 侧）。
//
// 分工不变：bridge 仍是 LX06 队列与命令仲裁的唯一权威；Songloft 只把
// 手机提交的 TrackRef 与命令转发过去，不建立第二队列、不持有设备状态。
// 未配置 `bridge_base_url` / 凭据时 Enabled()=false，BFF 退回本地状态机
// （迁移窗口行为不变）。
type BridgeRef struct {
	Source string         `json:"source"`
	ID     string         `json:"id"`
	Extra  map[string]any `json:"extra,omitempty"`
}

type BridgeClient struct {
	BaseURL string
	Token   string
	Target  string
	HTTP    *http.Client
}

func NewBridgeClient(baseURL, token, target string) *BridgeClient {
	return &BridgeClient{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		Target:  strings.TrimSpace(target),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *BridgeClient) Enabled() bool {
	return c != nil && c.BaseURL != "" && c.Token != ""
}

func (c *BridgeClient) target() string {
	if c.Target == "" {
		return "lx06"
	}
	return c.Target
}

func (c *BridgeClient) do(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.doWithStatus(ctx, method, path, body, out)
	return err
}

func (c *BridgeClient) doWithStatus(ctx context.Context, method, path string, body any,
	out any) (int, error) {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("bridge %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("bridge %s %s: http %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return resp.StatusCode, fmt.Errorf("bridge %s %s: decode: %w", method, path, err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(env.Data, out)
}

// CreateSession 开一个 bridge 托管会话（transport/backend 固定为已验证的 RPC 组合）。
func (c *BridgeClient) CreateSession(ctx context.Context) (string, error) {
	var snap struct {
		SessionID string `json:"session_id"`
	}
	err := c.do(ctx, http.MethodPost, "/api/music/sessions", map[string]any{
		"target":         c.target(),
		"transport":      "rpc",
		"player_backend": "mediaplayer",
	}, &snap)
	if err != nil {
		return "", err
	}
	if snap.SessionID == "" {
		return "", fmt.Errorf("bridge create session: empty session_id")
	}
	return snap.SessionID, nil
}

// Enqueue 把身份逐条入队（bridge 不接受 URL，只接受 TrackRef），
// 返回 bridge 分配的 entry_id 列表（顺序与 refs 一致），供 BFF 做 entry 映射。
func (c *BridgeClient) Enqueue(ctx context.Context, sessionID string, refs []BridgeRef) ([]string, error) {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		var out struct {
			EntryID string `json:"entry_id"`
		}
		err := c.do(ctx, http.MethodPost,
			"/api/music/sessions/"+sessionID+"/queue",
			map[string]any{"target": c.target(),
				"ref": map[string]any{"source": ref.Source, "id": ref.ID, "extra": ref.Extra}},
			&out)
		if err != nil {
			return nil, err
		}
		if out.EntryID == "" {
			return nil, fmt.Errorf("bridge queue: empty entry_id for %s/%s", ref.Source, ref.ID)
		}
		ids = append(ids, out.EntryID)
	}
	return ids, nil
}

// Command 下发命令；bridge 先落回执（202）再异步执行设备动作。
func (c *BridgeClient) Command(ctx context.Context, sessionID, kind, idemKey string,
	payload map[string]any, expectedEpoch *int64) (map[string]any, bool, error) {
	body := map[string]any{
		"target":   c.target(),
		"kind":     kind,
		"idem_key": idemKey,
	}
	if payload != nil {
		body["payload"] = payload
	}
	if expectedEpoch != nil {
		body["expected_epoch"] = *expectedEpoch
	}
	var receipt map[string]any
	status, err := c.doWithStatus(ctx, http.MethodPost,
		"/api/music/sessions/"+sessionID+"/commands", body, &receipt)
	if err != nil {
		return nil, false, err
	}
	// bridge 约定：202 = 首次执行，200 = 幂等重放（不会再驱动设备）
	return receipt, status == http.StatusOK, nil
}

// Snapshot 读取 bridge 会话状态（给 BFF 读路径用；不落库、不缓存）。
func (c *BridgeClient) Snapshot(ctx context.Context, sessionID string) (map[string]any, error) {
	var snap map[string]any
	if err := c.do(ctx, http.MethodGet,
		"/api/music/sessions/"+sessionID+"?target="+c.target(), nil, &snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
