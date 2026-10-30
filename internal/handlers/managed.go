// Package handlers — managed 托管播放入口（S-06/S-07/S-08 接线）。
// 全部 /managed/* 路由挂在 AuthMiddleware 之后（不进 publicPaths）；
// 唯一例外是 GET /media：票据即鉴权（设备播放器无法带 JWT 头）。
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"

	"songloft/internal/services/managed"
	"songloft/internal/services/requestidentity"
)

// ManagedHandler 托管播放 HTTP 面：BFF 命令、当前项准备、票据媒体、听音事件、ingest。
type ManagedHandler struct {
	bff     *managed.ManagedBFF
	manager *managed.Manager
	issuer  *managed.TicketIssuer
	events  *managed.EventLog
	ingest  *managed.IngestQueue
	media   *managed.MediaServer
	// ingestScan 单文件入库（app 装配期注入曲库扫描；nil 时 ingest 明确报错）。
	ingestScan managed.ScanFunc
	// bridge 是 BFF→bridge 的命令通道（B-08）；nil = 未配置，退回纯本地状态机。
	bridge    *managed.BridgeClient
	bridgeMu  sync.Mutex
	bridgeSID map[string]string // "<session>|<epoch>" -> bridge session id
}

// SetIngestScanFunc 注入曲库扫描实现（app 装配期调用一次）。
func (h *ManagedHandler) SetIngestScanFunc(f managed.ScanFunc) { h.ingestScan = f }

// SetBridgeClient 注入 bridge 命令客户端（未配置时保持 nil，BFF 只维护本地状态）。
func (h *ManagedHandler) SetBridgeClient(c *managed.BridgeClient) {
	h.bridge = c
	h.bridgeSID = map[string]string{}
}

// ensureBridgeSession 为某个 (session, epoch) 建 bridge 会话并把整队发过去。
// 同一代次只建一次；换代后旧 sid 不再复用（bridge 侧新会话接管，旧会话被停）。
func (h *ManagedHandler) ensureBridgeSession(ctx context.Context, session string, epoch int64,
	queue []managed.QueueEntry) (string, error) {
	key := fmt.Sprintf("%s|%d", session, epoch)
	h.bridgeMu.Lock()
	defer h.bridgeMu.Unlock()
	if sid := h.bridgeSID[key]; sid != "" {
		return sid, nil
	}
	sid, err := h.bridge.CreateSession(ctx)
	if err != nil {
		return "", err
	}
	refs := make([]managed.BridgeRef, 0, len(queue))
	for _, e := range queue {
		if e.Ref == nil || e.Ref.Source == "" || e.Ref.ID == "" {
			continue
		}
		refs = append(refs, managed.BridgeRef{Source: e.Ref.Source, ID: e.Ref.ID, Extra: e.Ref.Extra})
	}
	if len(refs) > 0 {
		if err := h.bridge.Enqueue(ctx, sid, refs); err != nil {
			return "", err
		}
	}
	h.bridgeSID[key] = sid
	return sid, nil
}

// forwardCommand 把 BFF 命令转发给 bridge（幂等键绑定 session/epoch/命令/entry）。
func (h *ManagedHandler) forwardCommand(ctx context.Context, session string, epoch int64,
	cmd managed.Command) (map[string]any, error) {
	key := fmt.Sprintf("%s|%d", session, epoch)
	h.bridgeMu.Lock()
	sid := h.bridgeSID[key]
	h.bridgeMu.Unlock()
	if sid == "" {
		return nil, fmt.Errorf("bridge session not established for this epoch")
	}
	kind := cmd.Name
	payload := map[string]any(nil)
	if cmd.Name == "jump" {
		kind = "play"
		payload = map[string]any{"entry_id": cmd.Entry}
	} else if cmd.Name == "play" && cmd.Entry != "" {
		payload = map[string]any{"entry_id": cmd.Entry}
	}
	idem := fmt.Sprintf("songloft-%s-%d-%s-%s", session, epoch, cmd.Name, cmd.Entry)
	return h.bridge.Command(ctx, sid, kind, idem, payload, nil)
}

func NewManagedHandler(
	bff *managed.ManagedBFF,
	mgr *managed.Manager,
	issuer *managed.TicketIssuer,
	events *managed.EventLog,
	ingest *managed.IngestQueue,
	media *managed.MediaServer,
) *ManagedHandler {
	return &ManagedHandler{bff: bff, manager: mgr, issuer: issuer, events: events, ingest: ingest, media: media}
}

// MediaHandler 暴露票据媒体端点（公开注册，票据自鉴权）。
func (h *ManagedHandler) MediaHandler(w http.ResponseWriter, r *http.Request) {
	h.media.ServeMedia(w, r)
}

func (h *ManagedHandler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return false
	}
	return true
}

// issuerIdentity 取认证主体作为 issuer；无主体（系统/内部调用）走显式系统身份。
func issuerIdentity(r *http.Request) string {
	if id, ok := requestidentity.FromContext(r.Context()); ok && id.Kind == requestidentity.KindUser {
		return "user:" + id.ClientID
	}
	return "system:songloft-managed"
}

// @Summary 提交托管播放队列
// @Description 手机/BFF 只提交 TrackRef 身份；bridge 是 LX06 队列权威，请求不得携带直链或长期凭证。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/epoch/items(source,id,extra)"
// @Success 200 {object} models.SuccessResponse
// @Failure 400 {object} models.ErrorResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 409 {object} models.ErrorResponse "代次过期"
// @Router /managed/queue [post]
// SetQueue POST /managed/queue：手机提交整队（仅 TrackRef 身份）。
func (h *ManagedHandler) SetQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Epoch   int64  `json:"epoch"`
		Entries []struct {
			Entry     string `json:"entry"`
			RefDigest string `json:"ref_digest"`
			Ref       *struct {
				Source string         `json:"source"`
				ID     string         `json:"id"`
				Extra  map[string]any `json:"extra"`
			} `json:"ref"`
		} `json:"entries"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	entries := make([]managed.QueueEntry, len(body.Entries))
	for i, e := range body.Entries {
		entry := managed.QueueEntry{Entry: e.Entry, RefDigest: e.RefDigest}
		if e.Ref != nil {
			// 队列里保留完整身份：BFF→bridge 转发只发 source/id/extra，不发 URL
			ref, err := managed.MakeRef(e.Ref.Source, e.Ref.ID, e.Ref.Extra, nil)
			if err != nil {
				respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if entry.Entry == "" {
				entry.Entry, _ = ref.Digest()
			}
			if entry.RefDigest == "" {
				entry.RefDigest, _ = ref.Digest()
			}
			entry.Ref = ref
		}
		entries[i] = entry
	}
	if err := h.bff.SetQueue(body.Session, body.Epoch, entries); err != nil {
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// @Summary 请求从手机原生 DLNA 交接到托管播放
// @Description 两段式 handoff 的第一段：登记期望代次与队列；旧续播职责未 Stop 前不允许起播。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/epoch/items"
// @Success 200 {object} models.SuccessResponse
// @Failure 400 {object} models.ErrorResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 409 {object} models.ErrorResponse "代次不一致"
// @Router /managed/handoff/request [post]
// RequestHandoff POST /managed/handoff/request。
func (h *ManagedHandler) RequestHandoff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	phase, err := h.bff.RequestHandoff(body.Session)
	if err != nil {
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"phase": phase})
}

// @Summary 确认旧客户端已显式停止
// @Description 两段式 handoff 的第二段：只有确认旧续播职责已 Stop 后才把阶段推进到 managed。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/epoch"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 409 {object} models.ErrorResponse
// @Router /managed/handoff/confirm [post]
// ConfirmHandoffStop POST /managed/handoff/confirm：旧续播职责已显式 Stop。
func (h *ManagedHandler) ConfirmHandoffStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Epoch   int64  `json:"epoch"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if err := h.bff.ConfirmHandoffStop(body.Session, body.Epoch); err != nil {
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	// managed 生效：把整队交给 bridge 并由 bridge 起播（Songloft 不持有设备队列）。
	if h.bridge != nil && h.bridge.Enabled() {
		state := h.bff.State(body.Session)
		sid, err := h.ensureBridgeSession(r.Context(), body.Session, body.Epoch, state.Queue)
		if err != nil {
			respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		_, err = h.bridge.Command(r.Context(), sid, "play",
			fmt.Sprintf("songloft-%s-%d-handoff-play", body.Session, body.Epoch), nil, nil)
		if err != nil {
			respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"ok": true, "bridge": map[string]any{"session_id": sid}})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// @Summary 执行托管播放命令
// @Description play/pause/resume/next/prev/stop：带幂等键与期望代次；回执区分 accepted/applied/observed/failed。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/epoch/command/idem_key/payload"
// @Success 200 {object} models.SuccessResponse
// @Failure 400 {object} models.ErrorResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 409 {object} models.ErrorResponse "幂等键冲突或代次过期"
// @Router /managed/command [post]
// Execute POST /managed/command。
func (h *ManagedHandler) Execute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		managed.Command
	}
	if !h.decode(w, r, &body) {
		return
	}
	if body.Session == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "session required"})
		return
	}
	if err := h.bff.Execute(r.Context(), body.Session, body.Command); err != nil {
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	var receipt map[string]any
	if h.bridge != nil && h.bridge.Enabled() && body.Command.Name != "" {
		var err error
		receipt, err = h.forwardCommand(r.Context(), body.Session, body.Command.Epoch, body.Command)
		if err != nil {
			respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "bridge": receipt})
}

// @Summary 托管会话换代
// @Description 换代后旧命令、旧票据与旧恢复资格整体失效；换代不恢复 owner。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/expected_epoch"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 409 {object} models.ErrorResponse
// @Router /managed/epoch [post]
// NewEpoch POST /managed/epoch：换代，旧命令/票据整体失效。
func (h *ManagedHandler) NewEpoch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"epoch": h.bff.NewEpoch(body.Session)})
}

// @Summary 读取托管会话状态
// @Description 返回阶段、代次、当前项与位置快照；读取需要独立 scope。
// @Tags 托管播放
// @Produce json
// @Security BearerAuth
// @Param session query string true "会话 id"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 404 {object} models.ErrorResponse
// @Router /managed/state [get]
// State GET /managed/state?session=
func (h *ManagedHandler) State(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "session required"})
		return
	}
	respondJSON(w, http.StatusOK, h.bff.State(session))
}

// @Summary 准备当前曲的固定表示
// @Description 只准备当前项：resolve→下载→校验≤320k MP3→冻结 final 表示，返回短期票据媒体地址；不预取队列。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/epoch/entry/ref"
// @Success 202 {object} models.SuccessResponse
// @Failure 400 {object} models.ErrorResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 422 {object} models.ErrorResponse "不可播放的候选（非 MP3/加密件/超码率）"
// @Router /managed/prepare [post]
// StartPrepare POST /managed/prepare：当前项准备（resolve→download→verify→ready）。
func (h *ManagedHandler) StartPrepare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Epoch   int64  `json:"epoch"`
		Entry   string `json:"entry"`
		Ref     struct {
			Source string         `json:"source"`
			ID     string         `json:"id"`
			Extra  map[string]any `json:"extra"`
		} `json:"ref"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	ref, err := managed.MakeRef(body.Ref.Source, body.Ref.ID, body.Ref.Extra, nil)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	op, err := h.manager.StartPrepare(issuerIdentity(r),
		managed.OpKey{Session: body.Session, Epoch: body.Epoch, Entry: body.Entry}, ref)
	if err != nil {
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]any{
		"state": op.State, "ref_digest": op.RefDigest,
	})
}

// @Summary 查询当前曲准备状态
// @Description 返回 pending/ready/failed；ready 时带票据化 media_url（固定表示，支持 Range）。
// @Tags 托管播放
// @Produce json
// @Security BearerAuth
// @Param session query string true "会话 id"
// @Param epoch query int true "代次"
// @Param entry query string true "队列项 id"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 404 {object} models.ErrorResponse
// @Router /managed/prepare [get]
// PrepareState GET /managed/prepare?session=&epoch=&entry=：
// 查询准备状态；ready 时签发票据并返回可播放的 /media URL。
func (h *ManagedHandler) PrepareState(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	entry := r.URL.Query().Get("entry")
	epoch, err := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
	if session == "" || entry == "" || err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "session/epoch/entry required"})
		return
	}
	op, ok := h.manager.Query(issuerIdentity(r), managed.OpKey{Session: session, Epoch: epoch, Entry: entry})
	if !ok {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "no such prepare op"})
		return
	}
	out := map[string]any{"state": op.State, "ref_digest": op.RefDigest,
		"resolved_source": op.ResolvedSource, "fallback_reason": op.FallbackReason}
	if op.Error != "" {
		out["error"] = op.Error
	}
	if op.State == managed.StateReady {
		tk, terr := h.issuer.Issue(r.Context(), managed.TicketRequest{
			RefDigest: op.RefDigest, Representation: "managed/" + op.RefDigest + "/final.mp3",
			Session: session, Epoch: epoch, Entry: entry, Target: "device",
			Subject: issuerIdentity(r),
		})
		if terr != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": terr.Error()})
			return
		}
		out["media_url"] = "/media?media_ticket=" + tk.Token
		out["lease_id"] = tk.LeaseID
		out["expires_at"] = tk.ExpiresAt.Unix()
	}
	respondJSON(w, http.StatusOK, out)
}

// @Summary 断连补偿的事件回放
// @Description 按 seq 增量返回听音事件（Last-Event-ID 语义）；缺口时返回 reset 让客户端重取快照。
// @Tags 托管播放
// @Produce json
// @Security BearerAuth
// @Param after query int false "已确认的最大 seq"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Router /managed/events [get]
// PendingEvents GET /managed/events?after=N：断连补偿 replay（Last-Event-ID 语义）。
func (h *ManagedHandler) PendingEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	evs, err := h.events.Pending(r.Context(), after, 200)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	maxSeq, err := h.events.MaxSeq(r.Context())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"events": evs, "max_seq": maxSeq})
}

// @Summary 确认事件消费进度
// @Description 有界清理已确认事件，避免 outbox 无限增长。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "session/upto_seq"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Router /managed/events/ack [post]
// AckEvents POST /managed/events/ack {upto_seq}。
func (h *ManagedHandler) AckEvents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UptoSeq int64 `json:"upto_seq"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if err := h.events.Ack(r.Context(), body.UptoSeq); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// @Summary 接收 WebGUI 下载完成通知
// @Description 根目录白名单、临时/加密件与未发布文件明确拒绝（422，不退化 500）；notif_id 幂等，通过后触发增量扫描。
// @Tags 托管播放
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body map[string]interface{} true "notif_id/identity/file_path"
// @Success 200 {object} models.SuccessResponse
// @Failure 401 {object} models.ErrorResponse
// @Failure 422 {object} models.ErrorResponse "路径外/临时件/未发布"
// @Router /managed/ingest [post]
// Ingest POST /managed/ingest：WebGUI completed outbox 接收端。
// 校验失败明确拒绝（rejected 落审计行）；通过后立即尝试处理一轮。
func (h *ManagedHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NotifID  string `json:"notif_id"`
		FilePath string `json:"file_path"`
		Identity string `json:"identity"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	state, err := h.ingest.Submit(r.Context(), &managed.IngestNotification{
		NotifID: body.NotifID, FilePath: body.FilePath, Identity: body.Identity,
	})
	if err != nil && !errors.Is(err, managed.ErrPathOutsideLibrary) &&
		!errors.Is(err, managed.ErrTempOrEncrypted) && !errors.Is(err, managed.ErrNotPublished) {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if state == managed.IngestRejected {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"state": state, "error": err.Error(),
		})
		return
	}
	done, failed, _, perr := h.ingest.ProcessAll(r.Context(), h.ingestScan)
	if perr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": perr.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"state": state, "scanned_done": done, "scanned_failed": failed,
	})
}

// RegisterRoutes 把 /managed/* 注册到鉴权组；/media 由调用方公开注册。
func (h *ManagedHandler) RegisterRoutes(r chi.Router) {
	r.Post("/managed/queue", h.SetQueue)
	r.Post("/managed/handoff/request", h.RequestHandoff)
	r.Post("/managed/handoff/confirm", h.ConfirmHandoffStop)
	r.Post("/managed/command", h.Execute)
	r.Post("/managed/epoch", h.NewEpoch)
	r.Get("/managed/state", h.State)
	r.Post("/managed/prepare", h.StartPrepare)
	r.Get("/managed/prepare", h.PrepareState)
	r.Get("/managed/events", h.PendingEvents)
	r.Post("/managed/events/ack", h.AckEvents)
	r.Post("/managed/ingest", h.Ingest)
}
