# S-06: 实现托管 BFF、手机 handoff 与听音事件

Status: ready-for-agent
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
