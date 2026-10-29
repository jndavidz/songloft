# S-03: 实现 TrackRef 当前项准备并禁止在线预取

Status: ready-for-agent
Type: task
Blocked by: 01
External blocked by: musicdl K-01/K-02（契约联调）

## Body

在 managed 路径落地 TrackRef 和当前项准备，复用 kwqq v2 身份与播放 profile。仅 Songloft 准备主体可取得标记 `requires_transcode` 的 AAC 规范化输入，AAC 不得直接 ready 或投送。搜索、候选导入、队列建立和客户端在线预取不触发直链解析；只有当前 entry 才解析、下载、必要转码和完整验证。remote 生命周期首版只覆盖准备状态与恢复，不做 GC。

### DoD

- B站 cid/page、咪咕 copyrightId 和 fallback 实际来源端到端保留；本地 song ID 与 TrackRef 不混用。
- 仅最终验证通过的 ≤320k MP3 进入 ready；原始、中间和最终表示隔离，失败不原码兜底。
- 20 项队列／导入在选中前 resolve=0，选中后仅当前项解析；显式元数据刷新等既有非 managed 行为不混淆。
- operation 查询／取消绑定 issuer 与 session/epoch/entry，迟到 ready 不可投送。

### Tests

- Go 复用音乐导入、metadata refresh、cache/transcode 测试；客户端复用 prefetch 与 queue 测试。
- 覆盖不同 cid/copyright、并发准备、取消、迟到 ready、非 MP3 和无下一项解析；AAC 正向规范化与禁止 AAC 直投均验，接入 no-prefetch/quality-profile。

### Rollback

关闭 managed prepare，保留 additive remote 状态和缓存；普通旧播放路径不变。

## Authorization

离线 fake resolver 与媒体 fixture；真实上游、客户端构建和 NAS 压测另行批准。

## Comments
