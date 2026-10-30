# S-05: 固定表示媒体服务与全局资源预算

Status: resolved（域内，2026-10-30：RFC 9110 语义与七分区预算落地；既有服务全量接线与 NAS 压测 Not run 见 Comments）
Type: task
Blocked by: 04
External blocked by: —

## Body

为票据媒体实现固定表示 GET／HEAD／single-Range，并统一 Songloft 本进程的缓存、实时、seek、电台、指纹、探测和 HLS 生产者的活跃与排队预算。musicdl／WebGUI 由 K-03/K-05 独立限额，首版采用静态分区，跨项目 runner 聚合两侧总量，不新增协调服务。首版完整准备，不用流式优化掩盖冷启动或表示问题。

### DoD

- 冷暖同 representation；206 字节与 Content-Range／Length 一致，416 带 `bytes */N`，If-Range 失配返回完整同一表示。
- HEAD 无响应体且不触发解析、下载或 ffmpeg；旧 URL 不换字节，失败不原码降级。
- Songloft 总在途与 waiter 有界，取消后线程／子进程真实退出才释放；当前播放与 stop 优先，满载明确 503；与 musicdl 汇总后不超静态分区总预算。
- 真实 AAC 样本和部署编码器测得完整准备链，不能用历史 FLAC 比率或 free 单点替代。

### Tests

- Range 多段拼接哈希、HEAD、If-Range、取消、队列饱和、子进程回收和 race 测试。
- NAS 性能仅在授权环境测量 CPU／RSS／I/O wait／吞吐；接入 representation/media-budget。

### Rollback

关闭 managed media serve 与新预算入口；保留缓存，不通过恢复原码或删除限流制造可用。

## Authorization

离线测试无需部署；真实 AAC、NAS 压测和资源限制变更分别批准。

## Comments

### 实施（2026-10-30，域内）

songloft bfbf2bc：固定表示语义（single-Range、多段回退 200 完整、416 bytes */N、If-Range
匹配/失配、ETag 表示身份、冷暖同字节、HEAD 无体无副作用）+ 七分区静态预算（FIFO 等待、
AcquirePriority 插队、满载 ErrBudgetFull→503、ctx 取消无泄漏、真实 worker 持槽断言）+
transcode 接 cache 分区。局限：既有 cache_service/HLS/电台生产者的全量分区接线是独立重构
切片；NAS 压测（CPU/RSS/I/O wait）需授权环境。

### 真机服务语义（2026-10-30）

- 设备侧实际请求走单段 Range（206/`content_range`），与离线矩阵（416、If-Range、
  多段回退 200、HEAD 无体）一致；冷暖同字节由离线用例锁定。
- **NOT RUN**：NAS 压测（预算分区在真实加载下的排队/503 行为，需专门窗口）。

### 范围裁决（2026-10-30 收尾）
DoD 的「七分区统一接线」**只完成托管转码分区**（`transcode.go` 走 `ProcessBudget`）；
既有 cache/HLS/电台生产者的全量接线是独立重构切片，**NOT RUN**。
本票按「预算原语 + 固定表示语义 + 转码接线」验收，不宣称全量预算已覆盖所有生产者。

### 全量接线与总闸门（2026-10-30 第二批，取代上一节的「全量接线 NOT RUN」范围）
上一节裁定的独立重构切片已在同日第二批完成，DoD 的「七分区统一接线」现为**已达成**：

- `resourcebudget` 新增进程级总闸门 `maxTotal`（GOMAXPROCS+2、下限 6、`SONGLOFT_MEDIA_MAX_TOTAL`
  可覆盖）与后台分区 `Reserve` 头寸；跨分区的等待者由 `dispatchLocked()` 全表扫描唤醒
  （一个等待者可能因**总数**而非本分区满而排队，解开它的槽来自别的分区释放）。
- 七分区全部接线，私有信号量删除：`seekStreamSem`（seek／音频 HLS／URL 直链三处共用）→
  seek/realtime 分区，`transcodeSem`（落盘转码／视频 HLS）→ cache/hls 分区，
  指纹逐文件取 fingerprint 槽，元数据补齐取 probe 槽（原先「每次调用各开一套 cap=2」）。
- 证据：`go test ./internal/services/resourcebudget/` 10 项 PASS（TotalCap／BackgroundReserve／
  TryAcquireFailsFast／TotalCapStress／DefaultWiredPartitions 为本批新增）；
  `hls_transcode_saturation_test.go` 改按预算断言饱和→503→释放→复用，并加验 `_total` 归零。
- **仍 NOT RUN**：NAS 真实加载下的压测（排队/503 行为、58091 暴露面）与现网部署——
  本批只在开发树验证，镜像未重建。
