// Package handlers — managed 托管播放入口（S-06/S-07/S-08 接线）。
// 全部 /managed/* 路由挂在 AuthMiddleware 之后（不进 publicPaths）；
// 唯一例外是 GET /media：票据即鉴权（设备播放器无法带 JWT 头）。
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

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
}

// SetIngestScanFunc 注入曲库扫描实现（app 装配期调用一次）。
func (h *ManagedHandler) SetIngestScanFunc(f managed.ScanFunc) { h.ingestScan = f }

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

// SetQueue POST /managed/queue：手机提交整队（仅 TrackRef 身份）。
func (h *ManagedHandler) SetQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Epoch   int64  `json:"epoch"`
		Entries []struct {
			Entry     string `json:"entry"`
			RefDigest string `json:"ref_digest"`
		} `json:"entries"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	entries := make([]managed.QueueEntry, len(body.Entries))
	for i, e := range body.Entries {
		entries[i] = managed.QueueEntry{Entry: e.Entry, RefDigest: e.RefDigest}
	}
	if err := h.bff.SetQueue(body.Session, body.Epoch, entries); err != nil {
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

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
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

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
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

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

// State GET /managed/state?session=
func (h *ManagedHandler) State(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "session required"})
		return
	}
	respondJSON(w, http.StatusOK, h.bff.State(session))
}

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
