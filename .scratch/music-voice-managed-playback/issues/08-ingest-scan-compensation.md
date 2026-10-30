# S-08: 实现下载完成增量扫描与失败补偿

Status: done（域内，2026-10-30：ingest 去重/拒绝矩阵/重试补偿落地；K-05 端到端联调 Not run 见 Comments）
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

### 实施（2026-10-30，域内）

songloft f448907：IngestQueue（migration 0043）——根目录白名单+穿越拒绝、临时/半文件/
加密件（ncm/kgm/mflac/mgg/qmc 系）永久拒绝且落审计行、未发布拒绝、notif_id 幂等（重复
投递一次入库）、乱序稳定序、有界重试（达上限 failed 保留现场）、pending 持久化重启续跑
（CompensateInterrupted+ProcessAll）。Not run：K-05 端到端联调（musicdl outbox→本队列）、
真实曲库扫描服务接线。

### 跨项目联调（2026-10-30，NAS 实网）

- `/api/v1/managed/ingest` 无凭据 401；路径外与「未发布文件」两条拒绝用例
  **显式 422**（runner `ingest-scan` PASS）。
- 修掉一个真机暴露的缺陷：被拒通知**重复投递**时 `ON CONFLICT DO NOTHING` 无返回行，
  `Submit` 直接回 `sql.ErrNoRows` → HTTP 500；现无论 pending/rejected 都回查既有状态，
  rejected 原样返回拒绝原因（commit `749bc5c`，新增 `TestIngest_DuplicateRejected`）。
- **NOT RUN**：musicdl outbox → 本队列的完整链路（需真实下载产物入库，避免留下
  死条目，未在曲库上执行）。
