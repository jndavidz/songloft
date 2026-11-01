# Spec: 音质档位路由（本地直出 / 网络源索取降档）

Status: ready-for-agent
Date: 2026-11-02
Source: 会话讨论（songloft 音质语义考证 + kwqq 链路现状 + 联网核验），核验记录见文末 Further Notes。

## Problem Statement

用户在 App 里把音质设置为 320kbps 后，得到的不是"向音源索取 320、拿不到就给实际最高档"，而是两件不符预期的事：

1. **本地歌曲被转码**：App 的音质设置对所有非视频歌曲统一追加 `quality=320`，后端把本地 FLAC/高码率源用 ffmpeg 转成 320k CBR mp3。本地音源本可以直接播放，用户不希望付出转码延迟与音质损失。
2. **网络歌曲走"下载+转码"而非"向源索取"**：对插件歌曲（kwqq 等），后端收到 `quality=320` 后先整首下载源文件再转码——付双倍带宽、首音被挂住数秒到 20+ 秒，且对音质毫无增益。而真正应该发生的事（带音质参数向源 API 索取、源侧返回 ≤ 档位的最高可用音质）其实已在上游存在，只是播放链路没有把档位送进去。

用户期望的语义：**本地音源永远直接播放（不转码）；网络音源把档位随解析请求带给插件，由插件/源 API 索取对应音质、不足时降档到实际最高可用。**

## Solution

把 App 的音质设置从「后端转码开关」改造为「网络源播放档位」：

- 本地歌曲播放请求**恒不带 quality**，后端原文件直出。
- 网络歌曲（插件音源）播放请求**同样不带 quality**（避免后端下载+转码），档位改为经音源解析契约 `/api/music/url` 的请求体下发给插件；插件用它向上游源 API 索取对应音质，上游不足时降档（kwqq/musicdl 侧语义已就位并经文档核实）。
- 档位词汇以 **musicdl 自研标准（ADR-0003 native tiers）为主词表**，MusicFree 四档作兼容别名；App 设置界面继续用用户熟悉的 128/192/320 数字档，传输层按 native 拼写透传、归一单点在上游 `quality.py`。
- 解析响应可选透传实测码率（`bitrate_kbps`），先落日志与 metrics，不做 UI（展示另立）。
- 缓存下载链路（离线缓存时定档转码）是合法的转码使用场景，**保持现状不动**。
- 全部改动落在共享 Dart 层 + Go 后端 + 插件 SDK/插件，**安卓端无原生层改动**：Android 与桌面/iOS/Web 共用同一 Flutter 播放内核与同一套 URL 构造逻辑，自动获得一致行为；安卓专项仅做真机冒烟验证（后台播放、通知栏、audio focus 不回归）。

## User Stories

1. As an Android app user, I want 音质设置只影响网络歌曲, so that 我的本地无损库永远原文件直出、不付出转码延迟。
2. As an Android app user, I want 播放插件歌曲时 App 自动向源索取我设置的档位, so that 不需要去每个插件自己的设置页单独配音质。
3. As an Android app user, I want 源上没有我请求的档位时自动拿到实际最高可用档, so that 播放不因档位缺失而失败或降为零选择。
4. As an Android app user, I want 插件歌曲首音不被"整首下载+转码"阻塞, so that 点播后的出声时间与本地歌同级。
5. As a desktop/Web/iOS app user, I want 音质路由行为与安卓完全一致, so that 同一账号在多端获得同一播放语义（共享 Flutter 内核，天然一致）。
6. As a plugin author, I want 解析请求里带上档位且不破坏旧插件, so that 未适配 quality 字段的插件照常工作、适配后即可参与档位路由。
7. As a kwqq plugin user, I want 档位被映射为 kwqq API 认识的拼写并向其索取, so that 上游"实测码率如实标注 + downgraded"的既有降档语义完整生效。
8. As an operator, I want 解析响应的实测码率进日志与 metrics, so that 排查"这首歌实际给了什么音质"不用抓包。
9. As a user who 离线缓存歌曲, I want 缓存下载仍按所选档位产出转码产物, so that 缓存文件体积/兼容性语义不变（缓存是有意定档的场景）。
10. As a user of 视频/电台/seek/变速/响度均衡, I want 这些既有链路完全不回归, so that 本次改动不触碰任何转码引擎行为。

## Implementation Decisions

- **D1 档位词汇与映射（主词表 = musicdl 自研标准）**：契约层以 musicdl ADR-0003 的 native tiers 为主词表：`auto`/`128k`/`192k`/`320k`/`flac`/`hires`/`master`/`surround51`；MusicFree 四档 `low`/`standard`/`high`/`super` 作为兼容别名（上游 `ALIASES` 表原样接受）。**播放 hint 合法集**限定为 `{128k, 192k, 320k}` + 四档别名——`auto` 不作为播放 hint 下发（其"best available"语义与 `320k` 封顶重叠，且避免依赖源实现差异）；无损四档（`flac`/`hires`/`master`/`surround51`）不下发（ADR-0002：MusicFree 插件消费能力 ≤320k mp3，无损属 webgui/下载域，受 `ENABLE_LOSSLESS` 门控）。防御规则：hint 携带非法/越权值时插件侧归一为 `320k`（宁可降档不可断播放）。App 设置存储值映射：`128→128k`、`192→192k`、`320→320k`、`original→不携带 quality hint`。**归一单点纪律**：拼写归一、192k snap（仅 netease 有真 192 档，其余源就近上取 320k）、per-source 白名单全部由 musicdl `quality.py resolve()` 单点完成，插件与 songloft 后端只透传拼写、不复制映射逻辑（ADR-0003："adapters must reference these constants instead of raw string literals"）。
- **D2 本地恒直出**：客户端对 `song.type == local`（含 Bundle 本地模式）构造播放 URL 时不追加 `quality`，也不下发 quality hint。后端转码引擎不动。
- **D3 播放 URL 不带转码 quality、网络歌带 `source_quality` hint**：客户端对**本地歌**构造 `/songs/{id}/play` 时不追加 `quality`；对**网络歌**（remote，含电台）同样不追加 `quality`，改为在音质设置 ≠ original 时追加独立参数 `source_quality=<native 拼写>`（`128→128k`、`192→192k`、`320→320k`）。统一播放端点（所有类型的 `song.url` 都是 `/songs/{id}/play`，models.go 单点生成）意味着客户端无需也无法区分插件歌——hint 对非插件网络歌**无作用点、被后端忽略**（无解析层，直出原流）。后端把 `source_quality` 视为哑管道：读取后仅透传给插件解析，**不参与转码判定**（参数名独立于既有 `quality` 转码参数，杜绝误触）。
- **D4 音源解析契约扩展（向后兼容）**：`/api/music/url` 请求体新增可选字段 `quality: string`（native 拼写；缺省或非法值 = 插件自定默认/防御归一）。响应新增可选字段 `bitrate_kbps: number`（上游实测码率，如实透传，仅日志）。Go 侧 `musicURLRequest`/`musicURLResponse` 加可选字段；未携带时行为与现状逐字节一致。运输链：客户端 `source_quality` → handler `servePlayOptions.sourceQuality`（哑管道）→ `CacheService/Orchestrator/Fetcher.ResolveURL` → 请求体 `quality`；`Fetch`（缓存下载/元数据探测路径）恒传空。
- **D5 SDK helper 扩展（向后兼容）**：`createMusicUrlHandler` 把 `body.quality` 经新增的第二个参数传给 `resolveUrl(sourceData, context)`，`context.quality` 可缺省；新增 `resolveQualityHint` 防御归一纯函数（SDK 2.19.0）。JS 无参强检，旧单参插件不受影响。**发布耦合说明**：kwqq 构建走 npm 已发布版 SDK，拿不到 2.19.0 导出——kwqq 侧内联同表归一函数（`QUALITY_TIERS` 七元集合 + 越权归 320k），SDK 与 kwqq 两处以 `quality.py` 为单点真相，待 SDK 发版后 kwqq 可切回导入。
- **D6 kwqq 插件透传**：kwqq 插件把合法集内的 hint 拼写**原样透传**给 kwqq v1 `/{source}/song/url`（`128k`/`192k`/`320k` 及四档别名——v1 容器与 v2 共用 `quality.py` 的 `ALIASES` 归一与 `REAL_192K` snap，插件不做本地复制）；命中防御规则的值归一 `320k` 后透传；未携带 hint 时维持插件自身配置 `cfg.quality`（默认 320k）不变。插件响应透传上游 `bitrate_kbps`。
- **D7 档位是播放期参数**：不落库、不进 source_data、不进缓存键语义；同一首歌不同档位每次解析独立进行（上游缓存归 kwqq API 管）。
- **D8 安卓适配边界**：改动全部在共享层（`core/audio` 的 URL 构造与播放器 provider），`just_audio`/media_kit 原生通道收到的最终 URL 语义不变；安卓无平台通道、权限、后台播放、通知栏改动。安卓专项 = 真机冒烟（后台播放 + 通知栏显示 + audio focus + 插件歌起播延迟目测改善）。
- **D9 缓存下载链路不动**：离线缓存下载继续携带现有 quality 参数走后端转码定档——那是"产出一份指定码率文件"的合法场景，与播放路由无关。**预热（prefetch）路径**：客户端按与播放相同的规则构造 URL（本地/电台不带任何音质参数、网络歌带 `source_quality`），后端预热分支不消费 hint——预热缓存落的是插件默认档的原生流（kwqq 默认即 320k，与 App 高档一致）；档位差异化解析只发生在真实播放的 `ResolveURL`。修正一处背景事实：未缓存插件歌的播放请求现状是"解析后原样代理"（`quality` 被忽略），受 `quality` 影响的是**已缓存副本与预热副本的转码**——本 spec 落地后两者都不再发生。

## Testing Decisions

- **Go 后端**（既有 seam：`internal/services/source/fetcher_test.go` 的 mock `PluginInvoker`）：断言 `musicURLRequest` 的 `quality` 字段——播放链路携带 App 档位、缓存下载链路不携带（或按 D9 现状）、缺省时请求体与旧版一致。
- **插件 SDK**（`plugins/toolchain/packages/plugin-sdk`）：`createMusicUrlHandler` 单测——`body.quality` 正确进入 `context.quality`；缺失/非法值时 `context.quality` 缺省；旧单参 `resolveUrl` 插件照常工作。
- **kwqq 插件**：透传与防御归一落为 SDK 纯函数 `resolveQualityHint`（`plugins/toolchain/packages/plugin-sdk`，node --test 离线单测：合法集原样透传 / 别名透传 / 非法与无损档归 320k / 缺省回落默认档）；kwqq 因发布耦合内联同表（见 D5），逻辑与 SDK 共表、真相在 `quality.py`，kwqq 侧无独立测试 seam，经 `node --check` + 插件构建验证。插件打包验证依赖真机窗口（与 S-07 既有真机授权一致）。
- **Flutter 客户端**（既有 seam：`test/core/audio/`，先例 `live_stream_headers_test.dart` 等）：按歌曲类型 × App 档位断言最终播放 URL——local 恒无 `quality=`；插件歌恒无 `quality=`（quality 走解析 hint）；缓存下载 URL 仍带 `quality=`；电台/视频既有分支不回归。
- **安卓真机冒烟**（device 窗口，非阻塞门）：插件歌起播延迟、后台播放、通知栏、audio focus 四项目测清单。

## Out of Scope

- **MusicFree 水平的插件契约扩展**（专辑页/远端歌单/歌手作品/排行榜/评论、MusicFree 插件适配器）——已另行评估可行，独立 spec，不混入本批。
- **实际播放音质的 UI 展示**（`bitrate_kbps` 本次只进日志/metrics）。
- **无损/`super` 档的 App 入口**（依赖上游 `ENABLE_LOSSLESS` 门控与容量预算，另行评估）。
- **后端转码引擎任何行为变更**（视频/电台/seek/变速/响度均衡/抽轨全部不动）。
- **musicdl kwqq API 侧任何改动**（四档归一、实测码率、downgraded 标注已就位，是本次的既有依赖而非交付物）。
- 192k 档在酷我源的假档问题（上游档位现实，spec 只保证如实映射）。

## Further Notes

**核验记录（2026-11-02 联网）**：

1. MusicFree 官方插件规范：`getMediaSource(musicItem, quality: IQualityKey)`，`IQualityKey = "low"|"standard"|"high"|"super"`，返回 `{url, headers?, userAgent?, size?}`（[MusicFreePluginTemplate types/plugin.d.ts:43,211-214](https://github.com/maotoumao/MusicFreePluginTemplate)——客户端带档位、插件/源侧自决降档，与 D1/D4 同构）。概述佐证：[MusicFree 插件化架构](https://blog.csdn.net/gitblog_00224/article/details/157080669)、[MusicFree 官方仓库](https://github.com/maotoumao/musicfree)。
2. Subsonic/OpenSubsonic 语义：`maxBitRate` 是**封顶**而非目标码率——源 ≤ 上限回原文件、超出才转码、"never up"（[OpenSubsonic 扩展讨论](https://github.com/opensubsonic/open-subsonic-api)、[Navidrome #2695](https://github.com/navidrome/navidrome/issues/2695)、[Airsonic #1138](https://github.com/airsonic/airsonic/issues/1138)）。佐证 D2（本地直出）与 D3（播放期封顶语义）；同时说明 songloft 现行"quality= 按请求码率无条件产 CBR"是偏离业界惯例的设计，本 spec 不改转码引擎、只在路由层绕开。
3. musicdl v2 四档归一与降档证据链：`API-REFERENCE.md` §quality 路由（MusicFree 四档归一表、`delivered_quality` 按实测码率推导、`downgraded` 如实标注）——kwqq 链路"索取后降档"的既有事实源。
4. musicdl 自研档位标准（ADR-0003 单点权威，`server/quality.py` 源码级核验）：native tiers 主词表 `auto/128k/192k/320k/flac/hires/master/surround51`；`ALIASES` 接受 MusicFree 四档与 `5.1` 等别名；`REAL_192K = {netease}`（其余源 192k snap 到 320k——酷我 192kmp3 为假档实测降 128）；`ALLOWED_TIERS` per-source 白名单，越权回落 `auto`；`LOSSLESS_TIERS` 受 `ENABLE_LOSSLESS` 门控。本 spec D1/D6 的"主词表 + 透传 + 单点归一"即按该文件纪律落笔。
5. 上游合并核验（2026-11-02，songloft HEAD = `5b710aa` Merge upstream/main）：用户合并的 6 个上游提交（miot 三件 / 曲库全选批量删除 / miot 插件 2026.11.1.1 / HLS FFmpeg 工具构建）经 `diff 54df447..5b710aa` 验证**均未触碰**本 spec 两个 seam（`clients/player/lib` URL 构造、`internal/services/source` 解析契约、插件 SDK/kwqq）；spec 假设仍成立。

**遗留语义说明**：App 上选 192 在 kwqq 系源实际拿到 320（上游 `REAL_192K` snap：仅网易有真 192 档，其余源就近上取），属 musicdl 单点归一的如实结果而非 bug；设置项文案建议标注"不足时取实际最高档"。

**Seams 选择**（to-spec 纪律，仅两处，均为既有高层 seam）：① 客户端播放 URL 构造（external behavior = 最终请求的 URL 形态）；② 音源解析 HTTP 契约（Go `musicURLRequest` ↔ 插件 `/api/music/url` body）。未新增任何 seam。
