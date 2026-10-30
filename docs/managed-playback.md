# 托管播放（managed playback）服务契约

> 跨项目主 spec：`open-xiaoai/.scratch/music-voice-managed-playback/spec.md`（B/S/K 票据图）。
> 本仓负责：当前项准备、固定表示与票据化媒体服务、听音事件、BFF 交接状态机、
> ingest（下载完成入库）。**LX06 队列与命令仲裁归 bridge**，本仓不建第二队列。
> 接口定义见 `docs/swagger.json`（`swag init` 生成，`/managed/*` 与 `/media`）。

## 所有权与边界

- TrackRef 是唯一稳定身份（源 + id + 必要 extra：bilibili cid/page、migu copyrightId）。
  调用方只发身份，不发 URL；上游凭据留服务端。
- bridge 独占设备队列与命令执行；手机只提交队列/命令与读取状态，**不建立第二队列**。
- 本机播放（手机原生 DLNA）与托管播放是两种所有权：交接必须两段式——先登记
  （`/managed/handoff/request`），旧续播职责显式 Stop 后再确认（`/managed/handoff/confirm`）。
  未确认 Stop 前不得起播，避免双 owner。
- GENA 只作可选补充信号（bridge 侧 B-09），本仓不依赖设备事件判定完成。

## 当前项准备（S-03）

- 只准备当前曲：搜索/候选导入/队列预取都不解析在线直链。
- resolve（kwqq v2）→ 下载 → 校验（实际编解码、MP3 帧、时长、大小、逐帧码率 ≤320k）
  → 冻结 `managed/<digest>/final.mp3`。校验或转码失败即失败，**绝不回落原始字节**。
- AAC 只接受 `requires_transcode` 的归一化输入；其他调用方与加密件拒绝。

## 票据与媒体服务（S-04/S-05）

- 不透明短期票据，服务端只存 SHA-256；绑定身份、表示、session、epoch、entry、target。
- 续期只延长原票据，不改 URI/表示；绝对生命期到期或撤销立即终止在途读取。
- `/media` 支持 GET/HEAD 与单段 Range（206/416、If-Range、ETag=表示身份）；
  多段请求回退 200 完整响应；HEAD 不触发准备。
- 日志脱敏：票据与私有直链不落日志；禁止可重放的私有缓存。
- 资源预算为七分区静态配额（FIFO 等待、优先级插队、满载 503），另有一条**进程级总闸门**
  `maxTotal`（GOMAXPROCS+2、下限 6、`SONGLOFT_MEDIA_MAX_TOTAL` 可调）：分区只限本类生产者，
  活跃之和的硬上限由总闸门给；后台分区（fingerprint/probe）带 `Reserve` 头寸，不得饿死播放。
  七个分区已全部接线（落盘转码/managed prepare、realtime 直链、seek+音频 HLS、电台、指纹、
  元数据探测、视频 HLS 段生产），原私有信号量 `seekStreamSem`/`transcodeSem` 已删除。
  单进程只约束本进程，跨项目总量由验收 runner 聚合 `Report()`（含 `_total`）。

## 命令、事件与恢复（S-06）

- 命令带幂等键与期望代次：重复键返回原回执，异 payload 冲突，过期代次拒绝。
- 事件与状态同事务写入；`/managed/events?after=` 支持断连回放，缺口返回 reset
  让客户端重取快照；`/managed/events/ack` 有界清理。
- 重启恢复只还原元数据并把未完成准备置 interrupted——**默认不自动续播**。

## ingest（S-08）

- WebGUI 下载完成 → `POST /managed/ingest`。根目录白名单 + 穿越拒绝；
  临时/半文件与加密件永久拒绝（422，不退化 500）；`notif_id` 幂等，
  乱序稳定排序；有界重试后保留 failed 现场；重启后 pending 续跑。

## BFF → bridge 命令通道

- `BridgeClient`（`internal/services/managed/bridge_client.go`）把手机提交的 TrackRef 与命令
  转发给 bridge：`handoff/confirm` 成功后建 bridge 会话 → 整队（只发 source/id/extra）→ `play`；
  `/managed/command` 转发并回传 bridge 回执，幂等键 = `songloft-<session>-<epoch>-<命令>-<entry>`。
- 配置：`BRIDGE_BASE_URL` / `BRIDGE_MUSIC_TOKEN`（env 优先，回落 config 服务同名键）。
  未配置时 BFF 只维护本地状态机（迁移窗口行为不变）。
- 部署前提：容器 `user` 主组必须能被 DSM ACL 放行（`1028:101`），否则读不到 `/app/music`。

## ingest 的真实语义

- 回调必须走 `SongService.ScanAndImportIngest`（**同步**导入 + 跳过 10s 稳定性窗口）：
  只调 `Scanner.ScanFilesWithCueInDirs` 只收集文件、**不写曲库**；刚原子发布的文件按默认
  10s 窗口会被静默跳过。

## 可观测字段

- 请求日志 `method/path/status/dur_ms/remote`（`/media` 额外记录 range 与 content_range）。
- 会话状态：`phase/intent/epoch/current_index/current_entry_id/position`。
- 事件：`seq/kind/entry_id/receipt_id`，seq 严格递增。
- 预算拒绝、票据拒绝与 ingest 拒绝都有显式错误码，不静默降级。

## 回滚

- 关托管入口即回到 legacy 路径；迁移保持向后兼容（不删表/事件），
  DB 备份见部署记录；镜像回滚用 `songloft-fork:rollback-*` 标签。
