# S-05: 固定表示媒体服务与全局资源预算

Status: ready-for-agent
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
