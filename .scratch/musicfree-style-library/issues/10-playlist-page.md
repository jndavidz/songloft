# 10: 歌单详情页（封面 / 介绍 / 曲目）

**What to build:** 搜索「歌单」tab 点击歌单 → 歌单页：封面、介绍（标题/创建者/简介/曲目数）、曲目列表可播（add-remote dedup + hint 链）。走 kwqq-api 精装歌单端点或透传 playlist detail；精装能力仅 netease 全通，其余源按 501 如实提示。

**Blocked by:** 06（后端模式）、07（歌单 tab 入口）、08（详情页模板与点播链）。

**Status:** resolved（2026-11-03：SheetDetailPage（封面/创建者/简介/曲目数 + 曲目触底分页 playlist/track/all）；曲目点播同链；播放全部单次导入上限 300 防超长歌单压死；501 源明确提示。Node/Go 测试覆盖）

- [x] 歌单 tab 点击进歌单页，封面/介绍/曲目数一致
- [x] 曲目点击即播，在线歌自动入库不重复
- [x] 长歌单（数百曲）分页加载不卡死不重复
- [x] 501 源（非 netease 精装路径）给「该源暂不支持」提示
- [x] 插件 Node 测试 + server Go 测试覆盖

## Notes（2026-11-03 落地）

- 离线施工完成；真机 UI 冒烟 Not run（本会话无设备窗口）。
