package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"songloft/internal/database"
	"songloft/internal/models"
	"songloft/internal/services"
)

// PlayHistoryHandler 播放历史处理器。
type PlayHistoryHandler struct {
	service *services.PlayHistoryService
}

// NewPlayHistoryHandler 创建播放历史处理器。
func NewPlayHistoryHandler(service *services.PlayHistoryService) *PlayHistoryHandler {
	return &PlayHistoryHandler{service: service}
}

// parsePlayContext 从 query 解析并校验播放上下文，失败时已写好响应并返回 false。
//
// context_key 走 query 而不是路径参数：歌手/专辑名里含 "/"、"%" 等字符，
// 放进 URL 路径会有编解码歧义（前端路由出于同样原因也把分面 value 放在 query）。
func parsePlayContext(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	contextType := r.URL.Query().Get("context_type")
	contextKey := r.URL.Query().Get("context_key")
	if !services.IsValidPlayContextType(contextType) {
		respondError(w, http.StatusBadRequest,
			"不支持的 context_type，必须是 playlist、tag 或分面维度（artist/album/genre/year/decade/language/style）", nil)
		return "", "", false
	}
	if contextKey == "" {
		respondError(w, http.StatusBadRequest, "缺少 context_key", nil)
		return "", "", false
	}
	return contextType, contextKey, true
}

// GetPlayHistory 查询某播放上下文的最近播放记录。
// @Summary 查询播放上下文的播放历史
// @Description 返回指定播放上下文内最近播放过的歌曲，按最后播放时间倒序，含完整歌曲详情。
// @Description 「播放上下文」由 context_type + context_key 二元组标识：歌单为 (playlist, 歌单 ID)，自定义标签为 (tag, 标签 ID)，分面维度为 (artist, 歌手名) / (album, 专辑名) 等。
// @Description 同一上下文内按歌曲去重（重复播放只刷新时间并累加 play_count），最多保留最近 50 条，因此本端点不分页。
// @Description 记录由 POST /songs/{id}/played 在 type=play 时写入。歌曲从库中删除时其历史自动级联清理；歌曲仅被移出歌单时历史仍保留，客户端起播时自行判定失效。
// @Tags 播放历史
// @Produce json
// @Param context_type query string true "播放上下文类型" Enums(playlist, tag, artist, album, genre, year, decade, language, style)
// @Param context_key query string true "播放上下文标识：playlist 传歌单 ID，分面维度传该维度取值"
// @Param limit query int false "返回条数，缺省 50，上限 50"
// @Success 200 {object} models.PlayHistoryListResponse "成功返回播放历史列表"
// @Failure 400 {object} models.ErrorResponse "context_type 不支持或缺少 context_key"
// @Failure 500 {object} models.ErrorResponse "服务器错误"
// @Security BearerAuth
// @Router /play-history [get]
func (h *PlayHistoryHandler) GetPlayHistory(w http.ResponseWriter, r *http.Request) {
	contextType, contextKey, ok := parsePlayContext(w, r)
	if !ok {
		return
	}

	limit := services.MaxPlayHistoryPerContext
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l < limit {
		limit = l
	}

	entries, err := h.service.List(r.Context(), contextType, contextKey, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "获取播放历史失败", err)
		return
	}

	respondJSON(w, http.StatusOK, models.PlayHistoryListResponse{
		Items: entries,
		Total: len(entries),
	})
}

// ClearPlayHistory 清空某播放上下文的播放历史。
// @Summary 清空播放上下文的播放历史
// @Description 删除指定播放上下文内的全部播放记录，返回实际删除条数。上下文不存在或本就没有记录时返回 deleted=0，不视为错误。
// @Tags 播放历史
// @Produce json
// @Param context_type query string true "播放上下文类型" Enums(playlist, tag, artist, album, genre, year, decade, language, style)
// @Param context_key query string true "播放上下文标识：playlist 传歌单 ID，分面维度传该维度取值"
// @Success 200 {object} map[string]int "成功返回 {deleted: 删除条数}"
// @Failure 400 {object} models.ErrorResponse "context_type 不支持或缺少 context_key"
// @Failure 500 {object} models.ErrorResponse "服务器错误"
// @Security BearerAuth
// @Router /play-history [delete]
func (h *PlayHistoryHandler) ClearPlayHistory(w http.ResponseWriter, r *http.Request) {
	contextType, contextKey, ok := parsePlayContext(w, r)
	if !ok {
		return
	}

	deleted, err := h.service.Clear(r.Context(), contextType, contextKey)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "清空播放历史失败", err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}

// DeletePlayHistoryEntry 删除单条播放历史记录。
// @Summary 删除单条播放历史
// @Description 从指定播放上下文中删除某首歌的播放记录。典型用途：清理已被移出歌单、在历史面板里显示为失效的条目。
// @Tags 播放历史
// @Produce json
// @Param context_type query string true "播放上下文类型" Enums(playlist, tag, artist, album, genre, year, decade, language, style)
// @Param context_key query string true "播放上下文标识：playlist 传歌单 ID，分面维度传该维度取值"
// @Param song_id query int true "要删除的歌曲 ID"
// @Success 204 "删除成功，无内容"
// @Failure 400 {object} models.ErrorResponse "context_type 不支持、缺少 context_key 或无效的 song_id"
// @Failure 404 {object} models.ErrorResponse "该上下文中不存在此歌曲的播放记录"
// @Failure 500 {object} models.ErrorResponse "服务器错误"
// @Security BearerAuth
// @Router /play-history/entry [delete]
func (h *PlayHistoryHandler) DeletePlayHistoryEntry(w http.ResponseWriter, r *http.Request) {
	contextType, contextKey, ok := parsePlayContext(w, r)
	if !ok {
		return
	}

	songID, err := strconv.ParseInt(r.URL.Query().Get("song_id"), 10, 64)
	if err != nil || songID <= 0 {
		respondError(w, http.StatusBadRequest, "无效的 song_id", err)
		return
	}

	if err := h.service.DeleteEntry(r.Context(), contextType, contextKey, songID); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			respondError(w, http.StatusNotFound, "播放历史记录不存在", err)
			return
		}
		respondError(w, http.StatusInternalServerError, "删除播放历史失败", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetRecentPlayHistory 跨上下文查询最近播放记录。
// @Summary 查询最近播放（跨上下文）
// @Description 返回全部播放上下文聚合后的最近播放记录：按歌曲去重（同一首歌取最近一次）、按最后播放时间倒序，含完整歌曲详情。
// @Description 与 GET /play-history 的区别：本端点不要求 context_type/context_key，面向「我最近听了什么」的全局视角；
// @Description total 为去重后的歌曲总数（非本次返回条数），配合 limit/offset 可做触底分页。
// @Tags 播放历史
// @Produce json
// @Param limit query int false "返回条数，缺省 200，上限 200"
// @Param offset query int false "偏移量，缺省 0"
// @Success 200 {object} models.PlayHistoryListResponse "成功返回最近播放列表"
// @Failure 500 {object} models.ErrorResponse "服务器错误"
// @Security BearerAuth
// @Router /play-history/recent [get]
func (h *PlayHistoryHandler) GetRecentPlayHistory(w http.ResponseWriter, r *http.Request) {
	limit := services.MaxRecentPlayHistory
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o > 0 {
		offset = o
	}

	resp, err := h.service.ListRecent(r.Context(), limit, offset)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "获取最近播放失败", err)
		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// ClearRecentPlayHistory 清空全部上下文的播放历史。
// @Summary 清空全部播放历史
// @Description 删除所有播放上下文中的全部播放记录（跨上下文），返回删除条数。「最近播放」页的全局清空走本端点。
// @Tags 播放历史
// @Produce json
// @Success 200 {object} map[string]int "成功返回 {deleted: 删除条数}"
// @Failure 500 {object} models.ErrorResponse "服务器错误"
// @Security BearerAuth
// @Router /play-history/recent [delete]
func (h *PlayHistoryHandler) ClearRecentPlayHistory(w http.ResponseWriter, r *http.Request) {
	deleted, err := h.service.ClearAll(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "清空播放历史失败", err)
		return
	}

	respondJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}
