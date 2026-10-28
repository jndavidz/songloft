# 待办：投送 DLNA 时歌词不跟随（客户端进度源未切换）

> 状态：**待办（未施工）** · 提出：2026-10-28 · 范围：`clients/player-lynx`（Lynx 客户端）+ 交付链（web 包 / APK）
> 现象来源：用户真机 — Songloft 搜歌 → 播放 → 投 LX06，**歌词不滚动**；同一首歌**不投送、手机本地播放时歌词正常滚动**。

## 1. 结论（根因）

**歌词时间轴只订阅本地播放器进度，投送时本地播放器被暂停 → 进度冻结 → 歌词不动。**

证据链：

| 环节 | 事实 | 位置 |
|---|---|---|
| DLNA 位置回传 | 正常。经代理受控投送实测：`TrackDuration=00:04:29`，`RelTime` 4s → 14s → 19s 递增 | `musicdl/.state/dlna-proxy.mjs` 转发 `GetPositionInfo`（日志实测） |
| 客户端轮询 | 存在且工作：投送激活后每 2s 调 `getPlaybackState`，写入 `positionMs/durationMs/isPlaying` | `clients/player-lynx/src/features/player/store/dlna-store.ts` `pollPlayback()`（约 L59–80） |
| 歌词索引 | `lyric-store.currentIndex` 只由 `syncPosition(positionMs)` 更新；唯一的调用方是**本地播放器**的进度事件 | `lyric-store.ts:232`（定义）；`player-store.ts:1348`（播放事件）、`:1568`（会话恢复） |
| 逐字动画 | `KaraokeLine` 的 `previous/anchor` 全部来自 `usePlayerStore`（`currentTime/isPlaying/speed`） | `widgets/KaraokeLine.tsx`（约 L44–110） |
| 投送时 | `castTo()` 会 `pauseLocal()` 暂停本地播放器 ⇒ 上述两处进度源冻结 | `dlna-store.ts` `castTo` |

即：`dlna-store.positionMs` 有值但**没有任何歌词组件消费它**。

## 2. 修复方案（建议）

### 2.1 统一进度源（最小改动）

1. 在 `dlna-store` 内维护「最近一次样本 + 采样时刻」，对外提供**插值后的进度**（投送中且 `isPlaying` 时按 `positionMs + (now - at)` 外推，250ms 节拍），避免 2s 轮询造成的行间跳动；暂停时停止外推。
2. 投送激活期间，由该进度驱动 `useLyricStore.getState().syncPosition(pos)`（放在 `dlna-store` 的节拍里，或新建 `useEffectivePosition()` 选择器供 `LyricsView` 消费）。
3. `KaraokeLine` 的进度源改为「投送态 → DLNA 进度；本地态 → `usePlayerStore`」，保留现有插值与可见性逻辑。
4. 顺带核对全屏播放器的进度条/时间显示是否同样冻结（若也订阅 `usePlayerStore`，一并切换；不在本待办验收内则另记）。

### 2.2 边界条件

- 投送开始/结束（`disconnect`）时恢复本地进度源；
- 投送切歌：`castSong` 后 `positionMs=0`，歌词需重载（现有 `loadForSong` 应已覆盖，需验证）；
- 投送暂停：`GetPositionInfo` 位置不再增长，插值必须停；
- 本地播放回归：未投送时行为与现状逐字等价（单测覆盖）。

## 3. 交付路径与限制

| 路径 | 可行性 | 说明 |
|---|---|---|
| **Web UI** | ✅ 本机可做 | 改 Lynx 代码 → `pnpm build:web` + `copy-bundle-web --embedded` → 重建 full 镜像 → 部署 NAS；手机浏览器打开 `http://10.10.10.2:58091` 验证 |
| **Android APK** | ⚠️ 需要构建环境 | APK 把 Lynx bundle 打进 `assets/main.lynx.bundle`（`DemoTemplateProvider`）；本机无 Android SDK。可选：fork 加 GitHub Actions（ubuntu + Android SDK + `./gradlew assembleDebug`）产出 APK |
| **OTA bundle 更新** | ❌ 暂不可用 | `BundleUpdates` 需要服务端签名的 manifest（`bundle_update` + signature + url），签名不在我们手里 |

结论：修复必须**重新打包客户端**；建议先 Web UI 验证，再决定是否加 CI 出 APK（用户 2026-10-28 裁决：先记录待办，暂不施工）。

## 4. 验收

1. **Web UI**（或新 APK）：手机/桌面打开 → 搜歌 → 播放 → 投「小爱音箱 (kwqq 代理)」→ 歌词随播放滚动；暂停时停在当前行；恢复后继续；切歌后从新歌第一行开始。
2. 本地播放（不投送）歌词行为不变。
3. 单测：`clients/player-lynx` 下新增用例覆盖「投送态进度源选择」「暂停不外推」「disconnect 回退本地源」。
4. 观测：`dlna-store` 的 2s 轮询保持（已够用），前端插值节拍不引入明显 CPU 开销。

## 5. 参考（本次诊断实测）

- 受控投送脚本结果（2026-10-28）：`SetAVTransportURI` → `Play` 后
  `@5s RelTime=00:00:04`、`@15s 00:00:14`、`@20s 00:00:19`，`TrackDuration=00:04:29`；
- 代理日志动作分布：`GetTransportInfo 29 / GetPositionInfo 29 / Play 9 / Pause 6 / SetAVTransportURI 3` —— 说明轮询与投送流程均正常运行。

## 6. 风险

- 2s 轮询 + 插值是近似值，长曲目可能累积漂移（可用每次轮询校正，本方案已如此）；
- 投送的 `Seek` 后需重置锚点，否则短暂跳词；
- 改动触碰播放器核心订阅链，必须跑 `clients/player-lynx` 既有测试（full-player / mini-player / karaoke）；
- Web UI 与 APK 两份产物要分别验证，避免只修一边。
