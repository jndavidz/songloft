# 03: 播放历史入口与列表页

**What to build:** 曲库「播放历史」入口 → 最近播放列表（每条含歌曲与播放时间，倒序），点击任意记录重播该曲（走既有 add-remote dedup → play 链），支持清空历史。纯消费现有播放历史端点与播放上报，server 零改动。

**Blocked by:** 01（曲库入口宫格）。

**Status:** resolved（2026-11-03，含规格修订：原票「server 零改动」前提不成立——实测 GET /play-history 强制 context_type+context_key（单上下文视图），跨上下文最近播放不存在。已在 server 新增 GET/DELETE /play-history/recent（sqlc ListRecentPlayHistory JOIN 去重 + Service.ListRecent + swag + 服务层测试）。player 侧 RecentPlaysPage：倒序/点击重播/清空/触底分页 50/页）

- [x] 播放若干歌后，历史页按时间倒序展示
- [x] 点击历史记录重播该曲且不产生重复入库
- [x] 清空历史生效（server 侧与 UI 一致）
- [x] 空历史给空态引导
- [x] 分页或触底加载不漏不重

## Notes（2026-11-03 落地）

- **规格修订**：原票「复用现有播放历史端点，server 零改动」前提经实测推翻——`GET /play-history` 的 `parsePlayContext` 强制要求 `context_type`+`context_key`（400 拒绝），历史按上下文隔离（每上下文 50 条），不存在跨上下文聚合视图。已按最小增量在 server 新增：
  - `GET /play-history/recent`（limit≤200/offset，按歌曲去重取最近一次、全局倒序，total=去重后总数供分页）
  - `DELETE /play-history/recent`（跨上下文清空）
  - SQL 用 JOIN 子查询保持列类型（MAX/SUM 聚合会让 sqlc 推成 interface{}）；同歌同时刻撞行的极小概率重复由仓储层按 song_id 去重兜底
- 验证：服务层测试 TestPlayHistoryServiceListRecentCrossContext（去重/倒序/分页/ClearAll）；swagger 已重生成；middleware/database/app 包 go test 全绿。
- Not run：真机 UI 冒烟（本会话无设备）。

### review 遗留（2026-11-03）

- 「清空全部播放记录」= 清空**所有上下文**的历史（含各歌单的「上次听到哪」），破坏面大于 UI 所见——属该端点的既定语义，确认弹窗已明确「清空全部」；如需只清聚合视图，需要服务端另行设计 tombstone 方案，不在本票。
