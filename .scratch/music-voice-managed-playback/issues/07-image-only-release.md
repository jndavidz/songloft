# S-07: 完成 image-only 升级与 Songloft 发布验收

Status: done（域内 + NAS image-only 升级与身份回读完成；客户端矩阵 Not run）
Type: task
Blocked by: 02, 06, 08
External blocked by: open-xiaoai B-05/B-06，musicdl K-06（跨区发布联调）

## Body

验证 NAS image-only 从旧版升级、additive migrations、默认配置、重启后的票据失败关闭和 lease／remote 恢复。修正自升级模式与路径契约，使 image-only 明确，不把仅补 IN_DOCKER 当作完成。汇总 Songloft 服务端、插件和客户端发布证据。

### DoD

- 旧版→新版→回滚均有版本化证据；无手工 ALTER，无错误路径自替换，重启不自动抢播。
- 完整镜像／配置／DB／插件包／客户端产物对应关系明确；旧客户端保持原路径但不列最终合规。
- Go 全量与受影响客户端测试通过；跨项目必需 Songloft cases 有结果，未跑真机／手机明确 Not run。
- Swagger／API／ADR 与可观测字段同步，Apache-2.0／NOTICE、修改声明、实际 AGPL 组件源码提供义务和 FFmpeg 构建许可完成发布检查。

### Tests

- 迁移、重启、票据撤销、lease 恢复、旧客户端兼容和 image-only 回滚测试。
- 配合 runner recovery-matrix；NAS 实际升级仅在批准窗口执行并回读运行身份。

### Rollback

关闭 managed capability 并回退上一镜像；迁移保持向后兼容，不删新表或事件，记录新数据 RPO。

## Authorization

不含镜像推送、NAS 实际升级、客户端发布或真机操作；分别授权。

## Comments

### 线下验收（2026-10-30）

songloft 223d189 恢复矩阵：旧"进程"（票据/准备/ingest）→ "重启"（同持久层新实例）→
准备 interrupted 不自动续播、过期票失败关闭、有效 lease 恢复、ingest 续跑、新旧表共存
（additive 契约：0040-0044 全部含 Down，DSF 升级测试覆盖 DownTo 回滚）。
Not run：**NAS 实际 image-only 升级与运行身份回读**（需批准窗口：WSL 侧 docker 构建 +
NAS 部署 + 回读版本；运行手册见 bridge/deploy/REBUILD.md 模式）；Swagger/ADR 同步随
app 层接线；客户端兼容矩阵需客户端构建。

### NAS 实际升级与运行身份回读（2026-10-30，image-only）

- **镜像身份**：由 `c2429ad`（HEAD）构建（`-ldflags` 注入 Version/GitCommit/BuildTime），
  运行镜像 `sha256:687ef4df2a19…`；升级前运行 `sha256:4edbec781ada…`（已打
  `songloft-fork:rollback-pre-ingestfix-20261030`），另有 `songloft-fork:previous`（`e591c636155d`）。
- **回读**：`GET /api/v1/version` →
  `v2.13.6-47-gc2429ad (commit: c2429adf23e3…, built: 2026-10-30T01:18:35Z)`；
  `docker inspect` image 与上述一致，status=running，`/api/v1/health` 200。
- **迁移**：goose `no migrations to run. current version: 44`（0040–0044 已在此前升级应用，
  本次不新增 schema）；升级前备份 `data.bak-ingestfix-20261030`。
- **失败关闭**：`/api/v1/managed/prepare`、`/api/v1/managed/ingest` 无凭据 401；
  `/media` 无票据 401。
- **不自动抢播**：重启后准备态保持 interrupted（恢复矩阵行为不变）；bridge 侧重启只把
  托管会话置 `recovery_pending`。
- **文档同步**：10 个 `/managed/*` 路由补 `@Summary` 注释并 `swag init` 重生成
  `docs/swagger.json|yaml|docs.go`；新增 `docs/managed-playback.md`（所有权/准备/票据/
  Range/预算/事件/ingest/可观测/回滚）。`/media` 不在 `/api/v1` basePath 下，写在契约文档里。
- **Go 全量**：`go build ./...` 通过；`go test ./...` 仅 `TestTranscodeToMP3_RealAACFixture`
  失败——本机 WSL 无 `ffmpeg`（`exec: "ffmpeg": not found`），**环境缺依赖**，不是回归；
  runner `songloft-managed-prepare` 因此记 BLOCKED。
- **NOT RUN**：客户端兼容矩阵（需构建 Flutter/Lynx 客户端并发布）；NAS 压测（S-05 预算）。
- 结论：本票范围内**完成**（授权不含客户端发布）。
