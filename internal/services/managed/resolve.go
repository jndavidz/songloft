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

	"songloft/internal/httputil"
)

// kwqq v2 播放解析客户端（契约事实源：musicdl server/routes/playback.py +
// server/playback.py assemble）。Songloft 的准备主体是唯一调用方，
// 凭证用独立 scoped service token（playback:issue + media:prepare），
// 既有下载密钥不自动获得播放权限。

// ResolveRequest 是 v2 resolve 的请求：只接受 TrackRef + profile + freshness，
// 不接受调用者任意 URL。
type ResolveRequest struct {
	Ref       *Ref
	Quality   string // 原始 quality 白名单拼写由 kwqq 归一化
	Target    string // device | media_prepare
	Freshness string // allow_cache | revalidate
}

// Verification 如实区分验证等级与转码要求。
type Verification struct {
	Level                    string `json:"level"`
	RequiresByteVerification bool   `json:"requires_byte_verification"`
	RequiresTranscode        bool   `json:"requires_transcode"`
}

// ResolveResult 对齐 kwqq assemble() 的响应形状。
type ResolveResult struct {
	TrackRef          *Ref              `json:"track_ref"`
	ResolvedSource    string            `json:"resolved_source"`
	RequestedQuality  string            `json:"requested_quality"`
	DeliveredQuality  *string           `json:"delivered_quality"`
	Codec             string            `json:"codec"`
	URL               string            `json:"url"`
	Ext               string            `json:"ext"`
	SizeBytes         int64             `json:"size_bytes"`
	BitrateKbps       *int              `json:"bitrate_kbps"`
	DurationS         *float64          `json:"duration_s"`
	Cover             string            `json:"cover"`
	Headers           map[string]string `json:"headers"`
	Parser            string            `json:"parser"`
	PlatformTag       string            `json:"platform_tag"`
	Verification      Verification      `json:"verification"`
	Ready             bool              `json:"ready"`
	Verified          bool              `json:"verified"`
	FallbackReason    string            `json:"fallback_reason"`
	ResolvedAt        int64             `json:"resolved_at"`
	CacheExpiresAt    *int64            `json:"cache_expires_at"`
	UpstreamExpiresAt *int64            `json:"upstream_expires_at"`
	Cached            bool              `json:"cached"`
}

// ResolveAPI 是解析层 seam：离线测试用 fake，生产用 KwqqClient。
type ResolveAPI interface {
	Resolve(ctx context.Context, req ResolveRequest) (*ResolveResult, error)
}

// KwqqClient 调用 kwqq v2 POST /v2/playback/resolve。
type KwqqClient struct {
	BaseURL string
	Token   string // X-Service-Token；与下载 API_KEY 完全独立
	HTTP    *http.Client
}

// ResolveError 携带 kwqq envelope 的业务码（HTTP 200 + code 403/404 也是失败）。
type ResolveError struct {
	HTTPStatus int
	Code       int
	Msg        string
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("kwqq resolve: http=%d code=%d msg=%s", e.HTTPStatus, e.Code, e.Msg)
}

func (c *KwqqClient) Resolve(ctx context.Context, req ResolveRequest) (*ResolveResult, error) {
	if req.Ref == nil {
		return nil, fmt.Errorf("kwqq resolve: nil track_ref")
	}
	freshness := req.Freshness
	if freshness == "" {
		freshness = "allow_cache"
	}
	body := map[string]any{
		"track_ref": map[string]any{
			"source": req.Ref.Source,
			"id":     req.Ref.ID,
			"extra":  req.Ref.Extra,
		},
		"profile":   map[string]any{"quality": req.Quality, "target": req.Target},
		"freshness": freshness,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpClient := c.HTTP
	if httpClient == nil {
		// 全局代理感知 client（pitfalls「通用 HTTP Proxy」：外发请求不得自建裸 client）
		httpClient = httputil.NewClient(30 * time.Second)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/v2/playback/resolve", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Service-Token", c.Token)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kwqq resolve: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("kwqq resolve: read body: %w", err)
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &ResolveError{HTTPStatus: resp.StatusCode, Code: -1,
			Msg: fmt.Sprintf("non-envelope response (status %d)", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK || envelope.Code != 200 {
		return nil, &ResolveError{HTTPStatus: resp.StatusCode, Code: envelope.Code, Msg: envelope.Msg}
	}
	var out ResolveResult
	if err := json.Unmarshal(envelope.Data, &out); err != nil {
		return nil, fmt.Errorf("kwqq resolve: decode data: %w", err)
	}
	// kwqq 的 verified 语义（K-02）：恒 false，完整验证只属于 Songloft 准备链。
	// 若上游宣称 verified=true，按契约不可信处理，拒绝该结果。
	if out.Verified {
		return nil, fmt.Errorf("kwqq resolve: upstream claimed verified=true, which the v2 contract forbids")
	}
	return &out, nil
}
