# S-09: 后续实施非破坏性 remote 隔离与 GC 预览

Status: ready-for-agent
Type: task
Blocked by: 07
External blocked by: —

## Body

首版发布后识别孤儿 remote 与表示，默认 preview／tombstone。排除收藏、歌单、活动队列、lease、准备中、近期失败重试、显式下载和听音事件引用；首阶段只做可恢复隔离，不物理删除。

### DoD

- 重复运行幂等，所有排除条件有表驱动证据；活跃数据和 Subsonic 对照零触碰。
- 候选数量、影响、隔离与恢复清单可审计；legacy remote 不因播放一次或同名被自动合并清理。
- 物理删除不在本票，若未来需要另开 ticket 和明确授权。

### Tests

- 覆盖全部引用类型、重启、重复执行、恢复和误删负例；运行相关 Go 与客户端曲库回归。

### Rollback

停用 GC 并解除隔离；因无物理删除，不需重建媒体。

## Authorization

禁止直接 DELETE、清缓存目录或清理用户下载；永久删除另行批准。

## Comments
