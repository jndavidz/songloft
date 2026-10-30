package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"songloft/internal/database"
	"songloft/internal/models"
	"songloft/internal/services"
)

// CreatePersonalToken 创建永久访问令牌
// @Summary 创建永久访问 Token
// @Description 仅正常登录会话可操作。名称为 1 至 80 个字符。令牌永久有效，拥有管理员业务 API 权限，不能管理 Token。完整凭证仅本次返回，数据库只存哈希。重启或退出登录不使其失效。
// @Tags 认证管理
// @Accept json
// @Produce json
// @Param request body models.CreatePersonalTokenRequest true "令牌名称"
// @Success 201 {object} models.CreatePersonalTokenResponse "仅此次返回完整凭证，请妥善保存"
// @Failure 400 {object} models.ErrorResponse "名称或请求无效"
// @Failure 401 {object} models.ErrorResponse "未授权"
// @Failure 403 {object} models.ErrorResponse "需要正常登录会话"
// @Failure 500 {object} models.ErrorResponse "创建失败"
// @Security BearerAuth
// @Router /auth/personal-tokens [post]
func (h *AuthHandler) CreatePersonalToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req models.CreatePersonalTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "无效的请求数据", nil)
		return
	}
	token, err := h.authService.CreatePersonalToken(r.Context(), req.Name)
	if errors.Is(err, services.ErrPersonalTokenName) {
		respondError(w, http.StatusBadRequest, services.ErrPersonalTokenName.Error(), nil)
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "创建 Token 失败", nil)
		return
	}
	respondJSON(w, http.StatusCreated, token)
}

// ListPersonalTokens 列出永久访问令牌
// @Summary 获取永久访问 Token 列表
// @Description 仅正常登录会话可操作。返回含已撤销记录的元数据、真实总数及分页；不返回完整凭证或哈希。last_used_at 最多每分钟更新一次，null 表示尚未使用。
// @Tags 认证管理
// @Produce json
// @Param limit query int false "每页数量" default(20)
// @Param offset query int false "偏移量" default(0)
// @Success 200 {object} models.PersonalTokenListResponse "令牌管理信息"
// @Failure 401 {object} models.ErrorResponse "未授权"
// @Failure 403 {object} models.ErrorResponse "需要正常登录会话"
// @Failure 500 {object} models.ErrorResponse "查询失败"
// @Security BearerAuth
// @Router /auth/personal-tokens [get]
func (h *AuthHandler) ListPersonalTokens(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, offset := parsePagination(r, models.DefaultPaginationLimit, 0)
	resp, err := h.authService.ListPersonalTokens(r.Context(), limit, offset)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "获取 Token 列表失败", nil)
		return
	}
	respondJSON(w, http.StatusOK, resp)
}

// RevokePersonalToken 撤销永久访问令牌
// @Summary 撤销永久访问 Token
// @Description 仅正常登录会话可操作。撤销后后续认证立即失败，不中断已开始的请求。重复撤销成功，不存在的 ID 返回 404。无需请求体。
// @Tags 认证管理
// @Produce json
// @Param id path string true "令牌管理 ID，不是完整凭证"
// @Success 204 "撤销成功"
// @Failure 401 {object} models.ErrorResponse "未授权"
// @Failure 403 {object} models.ErrorResponse "需要正常登录会话"
// @Failure 404 {object} models.ErrorResponse "Token 不存在"
// @Failure 500 {object} models.ErrorResponse "撤销失败"
// @Security BearerAuth
// @Router /auth/personal-tokens/{id} [delete]
func (h *AuthHandler) RevokePersonalToken(w http.ResponseWriter, r *http.Request) {
	err := h.authService.RevokePersonalToken(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, database.ErrNotFound) {
		respondError(w, http.StatusNotFound, "Token 不存在", nil)
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "撤销 Token 失败", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
