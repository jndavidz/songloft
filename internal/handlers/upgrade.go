package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"songloft/internal/models"
	"songloft/internal/services"
	"songloft/internal/version"
)

// githubProxyConfigKey GitHub 更新代理配置的 config key
const githubProxyConfigKey = "github_proxy"

// binaryTempName 上传/下载的临时二进制文件名
const binaryTempName = "songloft.new"

// githubProxySetting GitHub 更新代理配置。
type githubProxySetting struct {
	Proxy string `json:"proxy"`
}

// UpgradeHandler 升级处理器
type UpgradeHandler struct {
	upgradeService *services.UpgradeService
	configService  *services.ConfigService
	dataDir        string // 数据目录，用于派生临时文件路径
}

// NewUpgradeHandler 创建升级处理器
func NewUpgradeHandler(upgradeService *services.UpgradeService, configService *services.ConfigService, dataDir string) *UpgradeHandler {
	return &UpgradeHandler{
		upgradeService: upgradeService,
		configService:  configService,
		dataDir:        dataDir,
	}
}

// binaryTempPath 返回临时二进制文件的完整路径。
func (h *UpgradeHandler) binaryTempPath() string {
	return filepath.Join(h.dataDir, binaryTempName)
}

// GetVersions 获取可用版本信息
// @Summary 获取可用版本信息
// @Description 获取正式版和测试版的版本信息
// @Tags 系统升级
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{} "成功返回版本信息"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持升级"
// @Failure 500 {object} models.ErrorResponse "获取版本信息失败"
// @Security BearerAuth
// @Router /upgrade/versions [get]
func (h *UpgradeHandler) GetVersions(w http.ResponseWriter, r *http.Request) {
	// 检查是否在 Docker 环境
	if !h.upgradeService.UpgradeSupported() {
		respondError(w, http.StatusForbidden, "当前部署不支持进程内升级，请更新镜像", nil)
		return
	}

	// 从查询参数获取 GitHub 代理前缀
	githubProxy := r.URL.Query().Get("github_proxy")

	// 获取正式版信息
	stableInfo, stableErr := h.upgradeService.FetchVersionInfo("stable", githubProxy)

	// 获取测试版信息
	devInfo, devErr := h.upgradeService.FetchVersionInfo("dev", githubProxy)

	// 构建响应
	response := map[string]interface{}{
		"current": map[string]string{
			"version":    version.GetVersion(),
			"git_commit": version.GitCommit,
			"build_time": version.BuildTime,
			"channel":    h.upgradeService.CurrentVersionType(),
			"build_type": h.upgradeService.CurrentBuildType(),
		},
	}

	if stableErr == nil {
		response["stable"] = stableInfo
	} else {
		response["stable"] = map[string]string{
			"error": stableErr.Error(),
		}
	}

	if devErr == nil {
		response["dev"] = devInfo
	} else {
		response["dev"] = map[string]string{
			"error": devErr.Error(),
		}
	}

	slog.Info("GetVersions", "current_version", version.GetVersion())

	respondJSON(w, http.StatusOK, response)
}

// CheckUpdate 检查是否有新版本
// @Summary 检查更新
// @Description 检查是否有可用的新版本
// @Tags 系统升级
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{} "成功返回更新检查结果"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持升级"
// @Failure 500 {object} models.ErrorResponse "检查更新失败"
// @Security BearerAuth
// @Router /upgrade/check [get]
func (h *UpgradeHandler) CheckUpdate(w http.ResponseWriter, r *http.Request) {
	isDocker := h.upgradeService.IsDockerEnvironment()

	// 从查询参数获取 GitHub 代理前缀
	githubProxy := r.URL.Query().Get("github_proxy")

	// 检查更新
	updates, err := h.upgradeService.CheckForUpdates(githubProxy)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "检查更新失败", err)
		return
	}

	// 提取最新版本信息（优先 stable，其次 dev）
	latestVersion := ""
	releaseNotes := ""
	if stableUpdate, ok := updates["stable"]; ok {
		latestVersion = stableUpdate.Version
		releaseNotes = stableUpdate.ReleaseNotes
	} else if devUpdate, ok := updates["dev"]; ok {
		latestVersion = devUpdate.Version
		releaseNotes = devUpdate.ReleaseNotes
	}

	// 构建响应（同时提供嵌套和扁平字段，方便前端解析）
	response := map[string]interface{}{
		"is_docker":          isDocker,
		"has_update":         len(updates) > 0,
		"current_version":    version.GetVersion(),
		"current_channel":    h.upgradeService.CurrentVersionType(),
		"current_build_type": h.upgradeService.CurrentBuildType(),
		"latest_version":     latestVersion,
		"release_notes":      releaseNotes,
		"current": map[string]string{
			"version":    version.GetVersion(),
			"git_commit": version.GitCommit,
			"build_time": version.BuildTime,
			"channel":    h.upgradeService.CurrentVersionType(),
			"build_type": h.upgradeService.CurrentBuildType(),
		},
		"updates": updates,
	}

	respondJSON(w, http.StatusOK, response)
}

// StartUpgrade 开始升级
// @Summary 开始升级
// @Description 开始升级到指定版本
// @Tags 系统升级
// @Accept json
// @Produce json
// @Param request body map[string]string true "升级请求 {version_type: stable|dev}"
// @Success 200 {object} models.SuccessResponse "升级已开始"
// @Failure 400 {object} models.ErrorResponse "请求参数错误"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持升级"
// @Failure 500 {object} models.ErrorResponse "升级失败"
// @Security BearerAuth
// @Router /upgrade/start [post]
func (h *UpgradeHandler) StartUpgrade(w http.ResponseWriter, r *http.Request) {
	// 检查是否在 Docker 环境
	if !h.upgradeService.UpgradeSupported() {
		respondError(w, http.StatusForbidden, "当前部署不支持进程内升级，请更新镜像", nil)
		return
	}

	// 解析请求
	var req struct {
		VersionType string `json:"version_type"`
		GithubProxy string `json:"github_proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "无效的请求参数", err)
		return
	}

	// 验证版本类型
	if req.VersionType != "stable" && req.VersionType != "dev" {
		respondError(w, http.StatusBadRequest, "无效的版本类型，必须是 stable 或 dev", nil)
		return
	}

	if err := h.upgradeService.ValidateVersionTypeForUpgrade(req.VersionType); err != nil {
		respondError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// 在后台执行升级
	go func() {
		if err := h.upgradeService.UpgradeBinary(req.VersionType, req.GithubProxy); err != nil {
			// 升级失败，错误信息已在 UpgradeProgress 中记录
		}
	}()

	respondJSON(w, http.StatusOK, models.SuccessResponse{
		Message: "升级已开始，请稍候...",
	})
}

// ResetToBaseImage 回退到底包版本
// @Summary 回退到底包版本
// @Description 将二进制文件回退到 Docker 镜像中的原始版本，然后重启服务
// @Tags 系统升级
// @Accept json
// @Produce json
// @Success 200 {object} models.SuccessResponse "回退已开始"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持回退"
// @Security BearerAuth
// @Router /upgrade/reset [post]
func (h *UpgradeHandler) ResetToBaseImage(w http.ResponseWriter, r *http.Request) {
	// 检查是否在 Docker 环境
	if !h.upgradeService.UpgradeSupported() {
		respondError(w, http.StatusForbidden, "当前部署不支持进程内回退，请切换镜像", nil)
		return
	}

	// 在后台执行回退
	go func() {
		if err := h.upgradeService.ResetToBaseImage(); err != nil {
			slog.Error("ResetToBaseImage failed", "error", err)
		}
	}()

	respondJSON(w, http.StatusOK, models.SuccessResponse{
		Message: "回退已开始，服务即将重启...",
	})
}

// GetUpgradeProgress 获取升级进度
// @Summary 获取升级进度
// @Description 获取当前升级任务的进度信息
// @Tags 系统升级
// @Accept json
// @Produce json
// @Success 200 {object} models.UpgradeProgress "成功返回升级进度"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持升级"
// @Security BearerAuth
// @Router /upgrade/progress [get]
func (h *UpgradeHandler) GetUpgradeProgress(w http.ResponseWriter, r *http.Request) {
	// 检查是否在 Docker 环境
	if !h.upgradeService.UpgradeSupported() {
		respondError(w, http.StatusForbidden, "当前部署不支持进程内升级，请更新镜像", nil)
		return
	}

	progress := h.upgradeService.GetProgress()
	respondJSON(w, http.StatusOK, progress)
}

// GetGithubProxySetting 获取 GitHub 更新代理配置
// @Summary 获取 GitHub 更新代理配置
// @Description 获取检查更新 / 升级时使用的 GitHub 代理前缀（如 https://ghfast.top/）。前端会记住上次使用的代理并在检查更新时自动带上。未配置时返回空字符串（直连）。
// @Tags 系统升级
// @Produce json
// @Success 200 {object} githubProxySetting "GitHub 更新代理配置"
// @Security BearerAuth
// @Router /settings/github-proxy [get]
func (h *UpgradeHandler) GetGithubProxySetting(w http.ResponseWriter, r *http.Request) {
	var cfg githubProxySetting
	if err := h.configService.GetJSON(githubProxyConfigKey, &cfg); err != nil {
		respondJSON(w, http.StatusOK, githubProxySetting{Proxy: ""})
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdateGithubProxySetting 保存 GitHub 更新代理配置
// @Summary 保存 GitHub 更新代理配置
// @Description 设置检查更新 / 升级使用的 GitHub 代理前缀（如 https://ghfast.top/）。设为空字符串则直连。仅持久化，不影响其它模块的全局 HTTP 代理。
// @Tags 系统升级
// @Accept json
// @Produce json
// @Param request body githubProxySetting true "GitHub 更新代理配置"
// @Success 200 {object} githubProxySetting "保存后的配置"
// @Failure 400 {object} models.ErrorResponse "请求格式错误"
// @Failure 500 {object} models.ErrorResponse "保存配置失败"
// @Security BearerAuth
// @Router /settings/github-proxy [put]
func (h *UpgradeHandler) UpdateGithubProxySetting(w http.ResponseWriter, r *http.Request) {
	var req githubProxySetting
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if err := h.configService.SetJSON(githubProxyConfigKey, req); err != nil {
		respondError(w, http.StatusInternalServerError, "保存配置失败", err)
		return
	}
	slog.Info("GitHub 更新代理已更新", "proxy", req.Proxy)
	respondJSON(w, http.StatusOK, req)
}

// UploadBinary 上传二进制文件升级（阶段一：上传+验证）
// @Summary 上传二进制文件升级
// @Description 上传 Songloft 二进制文件进行离线升级。上传后验证文件可执行并提取版本信息返回给前端，由前端判断是否需要二次确认（如跨通道升级）。确认后调用 /upgrade/upload/confirm 执行实际替换。
// @Tags 系统升级
// @Accept multipart/form-data
// @Produce json
// @Param file formData file true "Songloft 二进制文件"
// @Success 200 {object} models.UploadedBinaryInfo "上传文件的版本信息"
// @Failure 400 {object} models.ErrorResponse "请求参数错误或文件无效"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持升级"
// @Failure 500 {object} models.ErrorResponse "保存或验证文件失败"
// @Security BearerAuth
// @Router /upgrade/upload [post]
func (h *UpgradeHandler) UploadBinary(w http.ResponseWriter, r *http.Request) {
	if !h.upgradeService.UpgradeSupported() {
		respondError(w, http.StatusForbidden, "当前部署不支持进程内升级，请更新镜像", nil)
		return
	}

	// 限制请求体大小 200MB
	r.Body = http.MaxBytesReader(w, r.Body, 200<<20)

	if err := r.ParseMultipartForm(200 << 20); err != nil {
		respondError(w, http.StatusBadRequest, "解析上传文件失败，文件可能超过 200MB 限制", err)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "未找到上传文件", err)
		return
	}
	defer file.Close()

	// 写入临时文件
	tmpPath := h.binaryTempPath()
	out, err := os.Create(tmpPath)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "创建临时文件失败", err)
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		os.Remove(tmpPath)
		respondError(w, http.StatusInternalServerError, "保存上传文件失败", err)
		return
	}
	out.Close()

	// 设置可执行权限并测试（不触发进度更新）
	if err := h.upgradeService.ValidateBinary(tmpPath); err != nil {
		os.Remove(tmpPath)
		respondError(w, http.StatusBadRequest, "上传的文件不是有效的可执行文件", err)
		return
	}

	// 提取版本信息
	info, err := h.upgradeService.ExtractBinaryInfo(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		respondError(w, http.StatusBadRequest, "无法识别上传文件的版本信息", err)
		return
	}

	respondJSON(w, http.StatusOK, info)
}

// ConfirmUploadUpgrade 确认执行上传升级（阶段二：替换+重启）
// @Summary 确认执行上传升级
// @Description 确认对已上传的二进制文件执行升级替换。前端在收到 /upgrade/upload 响应后，若需要二次确认（如跨通道），用户确认后调用此接口。升级进度通过 /upgrade/progress 轮询。
// @Tags 系统升级
// @Produce json
// @Success 200 {object} models.SuccessResponse "升级已开始"
// @Failure 400 {object} models.ErrorResponse "未找到已上传的升级文件"
// @Failure 403 {object} models.ErrorResponse "非 Docker 环境不支持升级"
// @Security BearerAuth
// @Router /upgrade/upload/confirm [post]
func (h *UpgradeHandler) ConfirmUploadUpgrade(w http.ResponseWriter, r *http.Request) {
	if !h.upgradeService.UpgradeSupported() {
		respondError(w, http.StatusForbidden, "当前部署不支持进程内升级，请更新镜像", nil)
		return
	}

	// 检查上传文件是否存在
	if _, err := os.Stat(h.binaryTempPath()); os.IsNotExist(err) {
		respondError(w, http.StatusBadRequest, "未找到已上传的升级文件，请先上传", nil)
		return
	}

	go func() {
		if err := h.upgradeService.UpgradeFromUpload(); err != nil {
			slog.Error("UpgradeFromUpload failed", "error", err)
		}
	}()

	respondJSON(w, http.StatusOK, models.SuccessResponse{
		Message: "升级已开始，请稍候...",
	})
}
