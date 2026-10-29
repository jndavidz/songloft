# S-02: 实施插件版本门与短期可撤销 scope

Status: done（2026-10-30，离线施工完成；部署、真机与真实凭据轮换不在本票范围）
Type: task
Blocked by: 01
External blocked by: —

## Body

在后端安装、上传、商店安装、更新、自动更新和启动／懒加载边界执行 minHostVersion／capabilities 校验。插件凭证按插件主体和最小 scope 迁移为短期可撤销形式，持久撤销／代次覆盖缓存与重启；保留旧消费方短并存窗口。

### DoD

- 不兼容插件在任何执行入口失败关闭，安装记录可按产品决策保留但不能运行。
- 过期、撤销、越 scope 和重启后的旧凭证均按新策略拒绝；不再以重启或百年签名替代撤销。
- manifest 与 HTTP 授权一致；不宣称 QuickJS 全部安全限制因此完成。

### Tests

- 覆盖安装／更新／加载全边界、撤销缓存、重启、旧凭证并存与撤旧、权限绕路。
- 运行 auth、jsplugin、jsruntime 相关 Go 测试和 runner plugin-policy；客户端只验已检出实际代码，未构建产物记 Not run。

### Rollback

停用新 scoped 消费面并吊销新票，必要时停用受影响插件；不恢复百年 token 的 DB 撤销旁路。

## Authorization

不发布插件、不轮换真实凭据、不修改 NAS；真实迁移另行批准。

## Comments

### 实施（2026-10-30）

落地三块，均为离线可测、未触碰部署/真实凭据：

**1. 兼容门（安装/上传/商店/更新/自动更新/启动/懒加载）**

- `internal/jsplugin/compat.go`：`CheckManifestCompatibility` + `CheckPluginRecordCompatibility`
  校验 `minHostVersion`（宿主版本先归一化 v 前缀 / `git describe` 后缀再比较；`dev` 构建视为满足；
  无法解析的宿主版本失败关闭）与 manifest 新增的 `capabilities`（白名单
  `HostPluginCapabilities`：music-source / lyrics-provider / cover-provider / page /
  command / inter-plugin）。未知能力或格式非法的版本要求失败关闭。
- 入口：`PackageManager.InstallFromUploadWithOptions`（上传/文件/商店共用）、`Update`
  （手动更新与 `DownloadUpdate` 自动更新共用）、`Manager.LoadPlugin`（启动加载、懒加载、
  启用、热重载共用）。加载门从 ZIP 内 plugin.json 读取能力声明（DB 不新增列，避免与
  并发迁移冲突）；ZIP 缺失/不可读/解析失败同样失败关闭。安装拒绝时不落库；已存在的记录
  加载失败时保留记录并按现有逻辑标记 error，不可运行。

**2. 短期、按插件主体、可撤销的凭据**

- `internal/services/plugin_credentials.go`：插件票据按 `plugin:<entryPath>` 签发，默认
  1 小时（`PluginCredentialTTL`），claims 带 `Kind=plugin`、`PluginEntry`、`Scopes`、
  `Gen`（全局代次）与 `PluginGen`（插件代次）。
- 代次持久化在 `configs.plugin_credential_state`；`AuthService` 首次使用时把全局代次 +1，
  因此每个进程启动都会让旧签名失效（测试用第二个 AuthService 实例模拟重启）。插件更新、
  禁用、卸载与管理员撤销递增插件代次；撤销直接拒绝下一条请求，不依赖 DB 撤销查询旁路，
  插件票据也**不进** tokenCache。
- 删除 `GeneratePluginToken`（百年 token）。旧 `plugin-system` 票据只在持久化的兼容窗口
  （`plugin_credential_legacy_until`）内被接受；**升级**默认 14 天，**全新安装**直接关闭窗口。
  窗口内旧票据保留原有数据面行为（含插件自身路径），管理面始终不可达；窗口缺失/非法或
  `strict` 档立即拒绝。`POST /api/v1/jsplugins/revoke-all-credentials` 可立即吊销全部票据
  并关闭旧票据窗口。
- 内部 HTTP 调用改用独立 system 票据（`IssueSystemToken`，中间件写 system identity，且只
  接受回环来源），不再伪装成插件主体。

**3. manifest 与 HTTP 授权一致**

- `internal/plugincred`：按「方法 + 路径」判定 scope（songs/playlists/song-tags/
  play-history/proxy），管理面、配置面、扫描、升级、插件管理与未归类路径一律 403；
  插件只能访问自己的 `/api/v1/jsplugin/{entryPath}/*`。
- `middleware.AuthMiddleware` 对 `Kind=plugin` 做 scope 校验；用户票据不受影响。
- 签发侧 scope 来自 manifest（strict）或全插件 scope（legacy 兼容档）。档位通过
  `GET/PUT /api/v1/jsplugins/credential-policy` 管理（默认 auto：窗口内 legacy，
  到期 strict），切换档位立即吊销全部票据。`POST /api/v1/jsplugins/{id}/revoke-credentials`
  支持按插件撤销，`POST /api/v1/jsplugins/revoke-all-credentials` 支持全量撤销并关闭旧票据
  窗口。通用 `PUT /configs/{key}` 改这两个配置时由 `configHandler.SetOnConfigChanged`
  补齐同样的吊销副作用。Swagger 已重新生成。
- `plugin.getToken` / `plugin.getFileUrl` / serveFile 内部转发改用插件短期票据；静态页日志
  不再拼接 `access_token`（凭据不落日志）。

**测试（go1.27.0 / Windows；工作树基线 `33e5687` + 未提交 S-02 改动）**

验证对象是未提交工作树，不是某个 commit。施工期间并发会话的 0040 迁移一度非法（缺
`-- +goose` 注解），导致 `testutil.OpenMemoryDB` 全栈失败；曾在隔离副本（排除该文件）里
跑过一轮。并发会话提交 `33e5687` 后迁移恢复合法，以下结果为**主工作树**复跑。

- 新增：`internal/plugincred/scopes_test.go`、`internal/services/plugin_credentials_test.go`、
  `internal/middleware/plugin_scope_wiring_test.go`（真实中间件→路由接线）、
  `internal/jsplugin/compat_test.go`、`internal/jsplugin/credentials_test.go`。
- 覆盖：安装/更新/自动更新/加载拒绝不兼容插件；未知能力；scope 允许/拒绝、管理面与跨插件
  路径；撤销、全局撤销、重启、过期；旧票据窗口内接受且降级、窗口外/strict 拒绝；更新/
  禁用/卸载撤销旧票据；token 缓存过期重签；通配符展开。
- 结果：`plugincred`、`middleware`、`app` 全绿；`services` 定向凭据/认证用例全绿；
  `handlers` 定向凭据端点用例全绿；`jsplugin` 仅剩两个预存 Windows 路径用例失败
  （`TestResolveFSPath_MusicPath`、`TestResolveFSPath_AbsolutePath_NoPermission`，
  S-01 已记录为平台性预存失败，本票未触碰）。`go test ./internal/handlers` 的其余失败仍为
  S-01 记录的 ffmpeg/seek/transcode 环境性失败。`go build ./...` 当前被并发会话未完成的
  `internal/services/managed/ticket.go` 阻塞（与本票无关）；S-02 涉及的包单独 `go build`
  与 `go vet` 均通过。

**Not run**

- runner 新用例 `songloft-plugin-policy`（`open-xiaoai` 已加入 `music_ecosystem.py`）：
  Windows 侧无 python，WSL 侧无 go，未实际执行；其包集合（plugincred/middleware/jsplugin）
  已在本机直接运行并归因。
- 真实客户端、真机、NAS 部署、真实凭据轮换：按授权不在本票范围。

**产品解释（需知悉）**

- `capabilities` 采用「插件声明所需宿主能力」的语义（与 `host.getInfo().capabilities` 的
  能力协商一致）；现有 manifest 无该字段时视为不要求，保持兼容。
- 百年 `plugin-system` 票据不会在升级瞬间全部失效：升级后的有界窗口内仍可用（保留旧数据面
  行为，含插件自身路径，管理面不可达），窗口结束后彻底拒绝；全新安装没有存量消费方，窗口
  直接关闭。新签发的 scoped 票据严格按代次撤销/重启失效。如需立刻停用旧消费方，调用
  `POST /api/v1/jsplugins/revoke-all-credentials`。
- 兼容档（legacy scope）在升级后默认持续 14 天，之后自动 strict；全新安装从第一天起 strict；
  管理员可提前切换或全量撤销。
- 本票不宣称 QuickJS 沙箱的全部安全限制（堆、fetch 目的地、并发、响应上限）已完成。
