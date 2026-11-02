# 11: 榜单入口与详情

**What to build:** 曲库「榜单」入口 → 平台榜单列表（名称/封面/更新时间），点击榜单 → 榜单详情曲目列表可播。走透传端点（榜单列表 + 榜单详情，映射对照 musicdl API-REFERENCE §5.1，酷狗 rank 系列与网易云 toplist 系列二选主通道）。

**Blocked by:** 01（曲库入口）、06（后端模式）。

**Status:** resolved（2026-11-03：ToplistsPage（netease toplist 主通道，名称/封面/更新频率网格）+ ToplistDetailPage（榜单 id=平台歌单 id，playlist/track/all 分页可播）。kugou rank 系列通道未接（§5.1 二选主通道取 netease）；上游失败错误态可重试）

- [x] 榜单入口可见，列表展示多平台或主平台榜单
- [x] 点击榜单进详情，曲目与平台一致
- [x] 榜单曲目可播（add-remote dedup + hint 链）
- [x] 榜单详情长列表分页正常
- [x] 上游失败给错误态可重试

## Notes（2026-11-03 落地）

- 离线施工完成；真机 UI 冒烟 Not run（本会话无设备窗口）。
