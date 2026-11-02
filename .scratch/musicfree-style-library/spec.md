# Spec: 曲库 MusicFree 式在线浏览与播放增强（搜索多类型 / 专辑 / 歌手 / 歌单 / 榜单 / 评论 / 加歌单）

> Triage: ready-for-agent（2026-11-03，来源：用户对 APK 曲库与播放页的功能要求，参考成熟播放器 MusicFree 的界面与功能范式）
> 能力基线：上游 kwqq-api 已具备本文所需全部通道（musicdl 仓 docs/API-REFERENCE.md §4 精装端点 + §5 酷狗/网易云透传端点，2026-10-29 v1.12）。本 spec 不改 kwqq-api 契约，只消费。

## Problem Statement

APK 底部三个 tab（首页/曲库/设置）中，曲库目前只有面向**本地库**的浏览形态（分类聚合/文件夹/标签），而现网曲库实际全部是在线插件源（kwqq）的 remote 歌——在线内容的发现路径几乎缺失：

- 搜索只有单曲一种结果类型；没有搜索历史。
- 没有榜单、推荐歌单及其平台分类入口。
- 单曲搜索结果中的专辑名/歌手名点不进去；没有专辑页、歌手页、歌单页。
- 播放页没有评论入口，也没有「加入歌单」。

用户在手机上无法像用 MusicFree / 网易云那样「搜 → 看专辑/歌手 → 连播整张 → 收进歌单 → 看评论」。

## Solution

按 MusicFree 的信息架构重构曲库 tab：顶部搜索框（源标识默认 kwqq）+ 一排入口（推荐歌单/榜单/播放历史/本地音乐）；搜索结果页四类型 tab（单曲/专辑/作者/歌单，默认单曲）；新增专辑页（封面/介绍含发行时间与公司/曲目可播）、歌手页（介绍/单曲/专辑）、歌单页（封面/介绍/曲目）；推荐歌单带平台分类标签；榜单两级浏览；播放页增加评论入口与加入歌单。

能力链四层分工（kwqq 保持唯一在线解析层，既有定案不变）：

```
player 曲库 UI  →  server 薄代理（JWT 鉴权 + 插件 scope 判定）  →  kwqq 插件新端点（MusicFree 函数命名）  →  kwqq-api（精装端点 + kugou/netease 透传）
```

**在线浏览态不落库**：专辑/歌手/歌单/榜单/评论全部即时拉取，不写 songs 表；点播任意在线歌曲走既有 add-remote → play 链（dedup_key 复用，source_quality 音质 hint 链路已生产实证）。

## User Stories

1. As a 听众, I want 打开曲库 tab 看到顶部搜索框（带 kwqq 源标识）与「推荐歌单/榜单/播放历史/本地音乐」一排入口, so that 在线内容发现有一眼可用的导航。
2. As a 听众, I want 点击搜索框后看到我的搜索历史、逐条删除单条、点击任一条直接发起该词搜索, so that 重复搜索零输入成本。
3. As a 听众, I want 搜索历史清空后回到无历史占位态, so that 界面不残留空态困惑。
4. As a 听众, I want 搜索结果页第一排是搜索框、第二排是「单曲/专辑/作者/歌单」四类型 tab、默认停在单曲, so that 与 MusicFree 一致的搜索心智。
5. As a 听众, I want 切换类型 tab 时结果列表就地切换、保持同一关键词, so that 一次搜索四类结果免重输。
6. As a 听众, I want 单曲结果条目上显示专辑名并可直接点击, so that 从单曲一步跳到所属专辑。
7. As a 听众, I want 专辑页展示封面、介绍（含发行时间/发行公司，平台可得多少给多少）与官方曲序曲目列表, so that 整张专辑一目了然。
8. As a 听众, I want 专辑页曲目点击即播、支持整张顺序连播, so that 「听完整张」零操作。
9. As a 听众, I want 点击歌手名进入歌手页，看到歌手介绍、单曲列表与专辑列表, so that 顺藤摸瓜发现歌手作品全貌。
10. As a 听众, I want 歌手页的专辑列表可点进专辑页、单曲可直接播, so that 歌手→专辑→曲目三级浏览闭环。
11. As a 听众, I want 歌单页展示封面、介绍与曲目列表且可播, so that 平台歌单可直接消费。
12. As a 听众, I want 推荐歌单页先看到平台分类标签（如 华语/流行/摇滚/ACG），点分类拉取该类歌单列表, so that 按口味逛歌单而非大海捞针。
13. As a 听众, I want 榜单入口给出平台榜单列表（名称+封面+更新时间），点进榜单看曲目并可播, so that 热门内容一页直达。
14. As a 听众, I want 播放历史入口展示我最近的播放记录（时间+歌曲），点击任意记录重播, so that 「昨天那首」两步找回。
15. As a 听众, I want 本地音乐入口列出本地入库歌曲, so that 本地与在线内容在曲库内并存可辨。
16. As a 听众, I want 播放页有评论入口，点开看到当前歌曲的评论列表（热评优先，分页加载）, so that 听歌时有社区氛围。
17. As a 听众, I want 播放页有「加入歌单」操作，弹出歌单选择器（含新建歌单）, so that 好歌随手归档。
18. As a 听众, I want 浏览在线专辑/歌手/歌单页中的任意歌曲点播即响，无需手动先「加歌入库」, so that 浏览与播放之间无断点。
19. As a 听众, I want 某源不支持的能力（如酷我专辑曲序缺失）得到明确提示而非空白或卡死, so that 知道换源或换歌。
20. As a 听众, I want 长列表分页/触底加载, so that 大专辑/长榜单不一次压死。
21. As a 听众, I want 详情页返回后列表滚动位置与已加载内容保持, so that 返回不迷路。
22. As a 听众, I want 播放中的歌曲在浏览页有「正在播放」标识, so that 知道当前上下文。

## Implementation Decisions

**分层与职责**

- **kwqq 插件扩展（唯一在线解析层，内部消费 kwqq-api）**——新增端点按 MusicFree 函数命名，manifest 同步扩展 scope 声明：
  - 搜索扩展 `type` 参数：`music`（现有 `search` 精装端点）/ `album`、`artist`、`sheet`（内部走 kugou/netease 透传 `search?keywords=&type=` 或 `cloudsearch`，映射表 musicdl API-REFERENCE §5.1）。
  - 专辑：`album/info`——走 kwqq-api 精装 `/{source}/album?name=&artist=`（二段式：候选+首选官方曲序）；逐源能力差异如实透出（netease/kugou/migu/qianqian ✅；kuwo/qq 未接入 → HTTP 501 envelope，客户端提示「该源不支持」）。
  - 歌手：`artist/info` + `artist/works`——走透传（`artist/detail`+`artist/audios` / `artists`+`artists/songs`）。
  - 歌单：`sheet/info`——精装 `/{source}/playlist`（netease ✅）或透传 `playlist/detail`。
  - 榜单：`toplists` + `toplist/detail`——透传（`toplist`/`rank/list` + 对应 detail）。
  - 推荐歌单：`recommend/tags` + `recommend/sheets?cat=`——透传（`playlist/hot`+`top/playlist?cat=` / classify 系列）。
  - 评论：`music/comments`——透传 `comment/music?id=`（读评论；匿名经容器 cookie 注入，无登录态要求）。
  - 插件端点通用形态对齐既有 `POST /api/music/url` 契约风格：请求体带 `source`/`id`/`type` 等定位字段；插件内部不做第二套平台 API 逆向，全部委托 kwqq-api。
- **server 薄代理**——新增一组插件代理路由把上述端点暴露给 player：JWT 鉴权沿用现有中间件；插件 scope 判定沿用插件凭据授权机制（按 manifest 声明放行，未声明即拒）；server 不解析不缓存在线浏览载荷（评论/榜单等直接透传 JSON），仅做超时与错误包装。**server 不落库任何在线浏览数据**。
- **点播链**——在线页面的歌曲播放：先按现有 add-remote 入库（dedup_key 幂等，重复点播不产生重复行），再走 `GET /songs/{id}/play?source_quality=<档位>`（音质档位路由链路已于 2026-11-02 生产实证）。详情页的「播放整张/整个歌单」= 按序批量 add-remote + 队列连播，用同一链路。
- **播放历史**——复用现有播放历史端点（`GET /play-history` + 播放上报）；曲库新增入口 + 列表页（记录时间+歌曲、点击重播、可清空）。
- **本地音乐**——复用现有曲库查询，按歌曲类型过滤本地歌；入口即列表页（现网无本地歌时给空态引导去扫描）。
- **加入歌单**——复用现有歌单 API（列表/建单/加歌），player 侧做歌单选择器弹层（含输入新建）。不加新 server 能力。
- **搜索历史**——player 本地持久化（设备内，不上 server），去重上限截断（如 20 条），单条删除+整体清空。
- **player 信息架构**——曲库 tab 重构为：顶部搜索框（源标识默认 kwqq）→ 四入口宫格 → 各独立页面；搜索结果页与详情页为压栈导航；复用现有曲库既有视图（分类聚合/文件夹/标签）作为库内浏览保留项，不删除。
- **UI 参照**——MusicFree 的搜索页/详情页结构（hero 区封面+文案、列表区）；空态/加载态/错误态三态齐备；501 能力缺失类错误给出「该源暂不支持」提示。
- **音质**——在线点播默认带 `source_quality` hint（跟随 app 音质设置既有语义：仅网络歌生效、越权归一 320k），复用已实证的哑管道，不做新逻辑。

**关键不变量（承接既有定案）**

- kwqq 是唯一在线解析层：player 与 server 均不得直连 kwqq-api 或平台 API。
- 在线浏览不污染曲库：songs 表只在点播时经 add-remote 增行；评论/榜单/推荐数据零落库。
- 无损档不透传、越权归一 320k：沿用 quality.py 单点归一，插件与 server 不复制映射。

## Testing Decisions

- **kwqq 插件（Node 离线模拟）**：新端点各自一个测试文件，mock kwqq-api 响应（成功/501 能力缺失/上游超时三态），断言端点契约（请求体定位字段、响应形状、501 如实透出）。Prior art: kwqq 插件 music-url 的离线模拟测试（同一 test 目录、同一 mock 风格）。
- **server 代理（Go handler 测试）**：httptest 断言——JWT 必需、插件 scope 未声明即 403、501/超时的错误包装、在线载荷透传不落库（无 songs 增行）。Prior art: 歌曲 facets 与认证中间件的 handler 测试。
- **player（Dart 单测）**：搜索历史持久化（增删去重上限）、类型 tab 状态切换、add-remote 点播前的 dedup 判定逻辑。Prior art: 音质参数 resolver 的单测。UI 布局与真机行为走真机冒烟（沿用「音箱不得出声须授权」窗口约束；本 spec 的 UI 冒烟不涉音箱，手机本地即可）。
- **跨项目回归**：kwqq-api 本身零改动，不动 musicdl 测试套件；交付批跑 workspace 跨项目离线验收入口（`music_ecosystem.py --lab`）确认既有 case 零回归。

## Out of Scope

- 评论的**写**操作（发布/回复/点赞）——只做匿名读。
- 歌单/专辑「收藏到本地账号」、平台登录 UI、第三方账号绑定。
- MV/视频内容、直播、电台类目。
- kuwo/qq 的专辑曲序补齐（上游 501 如实暴露即可，补齐属 musicdl 仓工作）。
- 桌面端/iOS 端；本轮只做 Android player。
- songloft 本地库的 song_artists 关联补建（remote 歌写歌手关联是另一个独立改进项，与本 spec 的平台歌手页互不依赖——本 spec 歌手页数据全部来自平台）。

## Further Notes

- 上游能力与路径映射的唯一事实源：musicdl 仓 `docs/API-REFERENCE.md` §4（精装端点）/§5.1（MusicFree 插件函数 ↔ 透传端点映射速查，2026-10-28 v1.8 起建、v1.12 维护中）。插件实现时逐函数对照该表，勿凭记忆写路径。
- 逐源能力矩阵（决定详情页哪些源能看）：album 精装端点 netease/kugou/migu/qianqian ✅、kuwo/qq ◐；playlist 精装端点仅 netease ✅；透传端点仅 kugou/netease 有。**默认搜索源 kwqq 插件内部聚多源**，类型搜索与详情页在插件内按源能力路由（如歌单详情优先 netease），必要时在响应里带 `source` 标注让 UI 如实展示出处。
- 播放历史与「加入歌单」纯消费现有 server 能力，可作第一批独立交付（无上游依赖）；搜索多类型/详情页/榜单/推荐/评论依赖插件扩展，第二批；两批均可独立验收。
- 真机验收沿用既有口径：adb logcat 与 server/插件日志两侧对齐；起播延迟对比基线用 Downloads 里 10-30 旧包。
- 既有边界提醒：kwqq 插件 SDK 2.18.0 单参调 resolveUrl 会静默丢 hint 的评审结论——新端点不涉及 resolveUrl，但凡新增涉及 quality 透传的路径须自持 handler 模式（与 music/url 同款）。
