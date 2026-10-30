# S-06: 实现托管 BFF、手机 handoff 与听音事件

Status: done（域内，2026-10-30：BFF 状态机/handoff/听音事件/四源专辑身份落地；bridge 联调与真机 handoff Not run 见 Comments）
Type: task
Blocked by: 05
External blocked by: open-xiaoai B-05（命令／SSE 联调）

## Body

实现 Songloft 固定 BFF 与插件／标准 App 托管入口，bridge 始终是 LX06 队列权威。插件／BFF 专辑映射必须保留四源每项 id/source/copyright，缺身份明确失败并提供可见错误，不能只把错误导入变成专辑不可用。手机从原生 DLNA 转 managed 前先结束旧续播职责并显式 Stop 确认；旧 APK 无自动 handoff 时提示手动断开。听音事件区分准备、真实播放、进度、完成和错误，使用有界 outbox 与 replay／snapshot 补偿。

### DoD

- 手机仅发 TrackRef／命令，不建立第二 managed 队列；关 App 后 bridge 继续，本机播放不注册到 bridge。
- 旧 lease／epoch 命令和迟到事件无效；接管不双播，旧 App 不自动恢复 owner。
- 事件携带 TrackRef、entry、lease 和幂等键；重复不乱记历史，prepare／HEAD／Range／prefetch 不记已听；崩溃或长期断连可由有界 outbox＋replay／snapshot 补偿。
- 四源专辑每项身份完整，缺字段不会静默错配或整专辑无提示消失。
- 服务凭据不进 WebView，BFF 不加入 publicPaths。

### Tests

- Go BFF 状态机、事件幂等／补偿与历史测试；Flutter／Lynx 复用 queue、prefetch、resume 和插件 dispatch 测试。
- runner album-identity 覆盖四源；mobile-managed 验证 handoff、关 App 续播、乱序事件和无双 owner；真实手机结果单独记录。

### Rollback

撤销 managed lease、关闭 BFF／按钮，恢复旧插件路径；保留事件审计，旧 APK 手动断开。

## Authorization

不发布客户端、不操作真实手机或 LX06；这些验收另行批准。

## Comments

### 实施（2026-10-30，域内）

songloft a3d7a3b：ManagedBFF 两段式 handoff（未确认 Stop 不得播）、旧 epoch 全拒、换代
不恢复 owner、stop 优先并撤票；EventLog（幂等去重、recordable 白名单防预听为记已听、
Last-Event-ID replay、Ack 有界清理、migration 0042）；MapAlbum 四源逐项身份（单项缺字段
显式失败可见）。Not run：bridge SSE 联调（B-05/B-06）、Flutter/Lynx 客户端、真实手机
handoff、app 层 BFF 路由。

### 跨项目联调（2026-10-30，NAS 实网）

- bridge 侧托管命令通路已与 Songloft 准备链联调：`play/pause/resume/next/stop`
  经 bridge `/api/music/*` 落到设备，事件 `seq` 严格递增（回放 12 条：
  `session_opened/command/command_observed/playing/paused`）。
- **NOT RUN**：手机 App 驱动 BFF 两段式 handoff（需 Flutter/Lynx 客户端）；
  Songloft BFF → bridge 的命令客户端尚未实现（当前 chain 是 bridge → Songloft 准备链）。

### BFF→bridge 命令通道（2026-10-30 实施）

- 新增 `managed.BridgeClient`（create/enqueue/command/snapshot，Bearer music-control 凭据）；
  `handoff/confirm` 成功后**建 bridge 会话 → 整队（只发 source/id/extra）→ play**；
  `managed/command` 转发并返回 bridge 回执，幂等键绑定 session/epoch/命令/entry。
- `QueueEntry` 保留完整 `Ref`（此前只有 digest，无法转发）。未配置
  `BRIDGE_BASE_URL`/`BRIDGE_MUSIC_TOKEN` 时退回纯本地状态机（迁移窗口不变）。
- 真机证据：`bff-handoff` runner case PASS（Songloft 200 → bridge 会话 playing →
  设备拉 `/media` 206 → pause/stop 转发生效）。
- 仍未验：手机 App 客户端本身（Flutter/Lynx 构建与发布）。
