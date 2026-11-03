# 09: 歌手详情页（介绍 / 单曲 / 专辑）

**What to build:** 搜索「作者」tab 点击歌手 → 歌手页：歌手介绍（含头像/简介，平台可得多少给多少）、热门单曲列表（可播）与专辑列表（可点进 08 的专辑页）。走透传端点（歌手详情+歌手作品，映射对照 musicdl API-REFERENCE §5.1）；数据带 source 标注。

**Blocked by:** 06（后端模式）、07（作者 tab 入口）、08（专辑页模板与目的地）。

**Status:** resolved（2026-11-03：ArtistDetailPage（头像/简介/专辑数/单曲数 + 热门单曲可播 + 专辑列表进 08 专辑页）；作品分页（artist/songs + artist/album limit/offset）；无数据源给明确空态/错误态）

- [x] 作者 tab 点击歌手进歌手页，介绍/头像正常展示
- [x] 单曲列表可播（add-remote dedup + hint 链）
- [x] 专辑列表点击进入专辑页
- [x] 热门单曲与专辑列表分页/截断合理
- [x] 无该歌手数据的源给明确空态/错误态

## Notes（2026-11-03 落地）

- 离线施工完成；真机 UI 冒烟 Not run（本会话无设备窗口）。
- 第二批（2026-11-03）：歌手页 AppBar 加「播放全部」（仅单曲模式显示）——热门单曲首屏 300 起播后，按 `artistInfo` 的 `music_count` 后台补齐。插件 `artist/works` 不返回 total，故总数未知（`music_count` 为 0）时只播已加载页；补齐页固定 limit=30 以对齐插件 50 上限的 offset 换算。歌手作品类型通道（kugou audios 行 id 取 `hash_128||hash`）见 06 号票 Notes。
