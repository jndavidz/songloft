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

- 离线施工完成；真机冒烟已于 2026-11-03 补做（见下条）。注意：真机这例的榜 total ≤ 已加载页，**补齐分支未触发**（按设计提前返回），补齐链路的真机证据取自歌手页（09 号票）。
- 第二批（2026-11-03）：榜单详情页 AppBar 加「播放全部」——首屏导入 300 立即起播，后台按 `toplist/detail` 的 total 补齐到 500 上限（`RemotePlayback.startBackgroundCatchUp`，代次过期自动停）。补齐链路口径见 06 号票 Notes。
- 真机（2026-11-03，Redmi 22041211AC）：榜单列表页出 6+ 个榜（名称/封面/更新频率），进「飙升榜」点 AppBar ▶ 起播首曲并高亮当前行；服务端 `toplist/detail` 仅 1 次调用 = 该榜 total ≤ 已加载页，补齐按设计提前返回（无多余请求）。判据细节见 06 号票「真机冒烟」节。
