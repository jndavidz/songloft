# 10: 歌单详情页（封面 / 介绍 / 曲目）

**What to build:** 搜索「歌单」tab 点击歌单 → 歌单页：封面、介绍（标题/创建者/简介/曲目数）、曲目列表可播（add-remote dedup + hint 链）。走 kwqq-api 精装歌单端点或透传 playlist detail；精装能力仅 netease 全通，其余源按 501 如实提示。

**Blocked by:** 06（后端模式）、07（歌单 tab 入口）、08（详情页模板与点播链）。

**Status:** ready-for-agent

- [ ] 歌单 tab 点击进歌单页，封面/介绍/曲目数一致
- [ ] 曲目点击即播，在线歌自动入库不重复
- [ ] 长歌单（数百曲）分页加载不卡死不重复
- [ ] 501 源（非 netease 精装路径）给「该源暂不支持」提示
- [ ] 插件 Node 测试 + server Go 测试覆盖
