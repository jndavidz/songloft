# S-08: 实现下载完成增量扫描与失败补偿

Status: ready-for-agent
Type: task
Blocked by: 06
External blocked by: musicdl K-05（原子发布／outbox 联调）

## Body

接收 WebGUI completed outbox，按正式曲库根目录和身份执行增量扫描。只读取已原子发布的文件，排除临时、半文件和加密件；通知去重、失败重试与长期断连补偿复用有界状态，不建立新常驻服务。

### DoD

- 同一完成事件重复投递只产生一次逻辑入库；乱序、失败和重启后可补偿，无重复历史或半文件。
- 路径穿越、临时／加密件和未发布文件明确拒绝；正式本地文件可被 Songloft／Lyrion 各自索引。
- WebGUI 发布与 Songloft 扫描的版本、身份和恢复证据完整，API 播放 URL 不进入本地保真链。

### Tests

- 覆盖重复／乱序 outbox、进程中断、扫描失败、路径穿越、临时／加密件和恢复补偿。
- 与 K-05 联调 runner ingest-scan；kwqq 不可用时验证本地文件与 E7 保真边界。

### Rollback

关闭新通知入口并保留 outbox 供恢复；不删除已发布正式文件，不回滚成扫描临时目录。

## Authorization

仅隔离测试库和离线文件 fixture；不扫描或修改正式曲库，真实入库另行批准。

## Comments
