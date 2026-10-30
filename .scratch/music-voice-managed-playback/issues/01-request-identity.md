# S-01: 统一请求身份并修复播放活动隔离

Status: resolved（2026-10-29，离线施工完成；真机/部署不在本票范围）
Type: task
Blocked by: —
External blocked by: —

## Body

建立共享 request identity，middleware 唯一写入，playactivity 与其他服务统一读取。系统任务显式使用 system identity，真实认证请求缺主体不得进入共享零值桶。保留 Activate 现有业务语义，不把修复扩大成取消全部 prefetch。

### DoD

- 两个真实认证客户端的异曲 play/transcode/reassign 不互取消；同 session 旧工作仍正常取消。
- 同曲工作和全部 prefetch 按现行规则保留；系统任务与用户桶可区分、可诊断。
- 所有生产调用点迁移，服务层不反向 import middleware。

### Tests

- 测试必须经过真实 middleware→handler→Registry 接线，不再只自造字符串 context key。
- 覆盖双客户端、同会话、系统任务、缺身份和 prefetch 矩阵；运行相关 Go 全量与 runner client-isolation。

### Rollback

回退上一构建并关闭 managed 路径；不得仅改回字符串 key 或恢复共享零值桶。

## Authorization

本地 Go 代码与测试；不改部署、token 权限或客户端产物。

## Comments

### 实施（2026-10-29）

缺陷根因：`middleware/auth.go` 用类型化私有 key `contextKey("client_id")` 写入 ctx，
`playactivity/registry.go` 用裸 string key `"client_id"` 读取——类型不同永远取不到值，
**所有**认证请求都落进共享零值桶（SessionKey{}），两个真实客户端互取消对方工作。

落地：

- 新增 `internal/services/requestidentity`：Identity{Kind: user|system, ClientID}、
  `User()`（缺主体报错）、`System()`/`SystemContext()`、ctx 读写与 Describe 诊断。
  middleware 与服务层都只 import 本包（服务层不反向 import middleware）。
- `middleware/auth.go`：唯一写入方；认证成功后 `requestidentity.User(claims.ClientID)`，
  **缺主体 401 失败关闭**；`ClientIDFromContext` 改为经 requestidentity 读取（行为不变）。
- `playactivity/registry.go`：SessionKey 增加 System 字段与 String()（user:/system/anon:
  可诊断）；`SessionFromContext` 统一经 requestidentity 读取；缺身份（未走鉴权的公开
  路径、未声明身份的内部调用）→ 一次性独立 anon 桶，禁止共享零值桶。
- `source.ReassignSessionKey` 增加 System 字段，adapter 双向映射保真。
- `Activate` 业务语义未动：同曲工作 + 全部 prefetch 保留、仅同会话取消（songloft#271/#300 语义锁定）。

测试：

- 缺陷证明：`internal/middleware/auth_identity_wiring_test.go` 真实
  AuthMiddleware→handler→Registry 接线（真实 AuthService + :memory: SQLite + chi 路由），
  修复前 `TestWiring_TwoAuthenticatedClientsDoNotCancelEachOther` 失败（跨客户端取消复现）、
  修复后全绿（双客户端隔离 / 同会话取消 / prefetch 存活 / 缺主体 401 且零注册 / whoami 主体）。
  注：缺主体用例在缺陷版本上经「DB 撤销检查报错→401」incidental 通过，非缺陷版本行为差异点；
  断言锁的是契约（401 + 不注册任何工作）。
- `registry_test.go` 去掉自造字符串 key，改经 requestidentity；补缺身份独立桶、
  系统桶隔离、缺主体 user 身份防御矩阵。

验证证据（Windows 侧，go1.27.0）：

- `go test ./internal/middleware/ ./internal/services/playactivity/
  ./internal/services/requestidentity/ ./internal/services/source/` 全绿。
- `go vet ./internal/...` 无告警。
- `go test ./...`：handlers/jsplugin/services 各有失败，**经 stash 后在 HEAD 复跑同环境归因
  为预存环境性失败**（本机无 ffmpeg 的转码/seek 用例 + Windows 路径语义用例），与本票改动无关。
- runner：`music_ecosystem.py` 新增 `songloft-client-isolation` case（定向三包，默认 lab 纳入），
  实测 PASS（ok_pkgs=3）；`songloft-offline`（go test ./...）在本机如实 BLOCKED（ffmpeg 缺失），
  需在 WSL/带 ffmpeg 环境跑全量作最终验收。

Not run：真实客户端验证。

### Not run 收口（2026-10-29 补）

- 本机补装 ffmpeg 后（`D:\PortableApps\_AV\ffmpeg-9.0.2-full_build`，gyan 9.0.2 构建）复跑
  `go test ./...`：失败集合缩小为 **Windows 平台性预存失败**——`/bin/echo` Unix 路径假设
  （StreamSeekedMP3*）、POSIX 权限语义（jsplugin ResolveFSPath*）、ffmpeg 9.0.2 构建变体
  （ExtractCoverFromVideoFile*）、seek/transcode 流式用例的平台假设（GetSongPlaySeek*/…Live）。
  均位于本票未触碰的包，与改动无关（置信度高：stash 归因 + 失败点全在平台假设上）。
  **全量绿需 Linux 环境**：WSL 侧当前无 go（用户工具链基线），是否安装由用户决定。
- runner 连带修复一个 Windows 缺陷：`subprocess text=True` 在 Windows locale 下遇 go 测试输出
  非 UTF-8 字节会解码崩溃 → `proc.stdout=None` → TypeError；已在 open-xiaoai `e068792` 修复
  （全 subprocess 显式 utf-8/errors=replace）。修复后 songloft-offline case 如实 FAIL 并给出
  上述归因清单。

### code-review 收口（2026-10-29，Standards/Spec 双轴并行）

采纳修复：

- `SessionKey.String()` anon 桶误标 `user:anon:<n>` → SessionKey 增加 unexported `anon`
  标记，String() 如实输出 `anon:<n>`；外部构造的空 ClientID 桶显示 `user:<empty>`。
- `requestidentity.Describe()` 无生产调用方（Speculative Generality）→ 删除。
- wiring 测试补系统身份真实接线用例（`TestWiring_SystemWorkIsIsolatedFromUserSessions`，
  系统工作挂无鉴权独立路由=后台任务真实拓扑，断言用户 Activate 不取消系统桶工作）。
- 测试 http body 读取改 `io.ReadAll`（消除 4096 字节短读风险）。

裁决不改：

- 缺主体 token 改 401（spec 只要求「不进共享零值桶」）：fail-closed 是更强且符合 spec
  安全姿态的解释；生产 token 的 client_id 由 `generateClientID()` 随机生成、plugin-system
  固定值，均非空，无存量破坏面。
- 缺身份 anon 桶按调用一次性派发：生产 `SessionFromContext` 调用点全部位于鉴权路由之后
  （公开路径是插件 Subsonic /rest/*，不经过这些 handler），同请求多次取用需关联的场景
  必须先建立身份——已在 registry.go 注释明示。
- 系统桶为单一共享桶：当前无生产系统 Track/Activate 调用点，Activate 只由用户请求触发；
  未来系统任务互取消问题留给引入真实系统工作时的 ticket。