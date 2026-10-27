# songloft-plugin-kwqq

Songloft 音源插件：把自建 [kw-qq-music-api](https://github.com/jndavidz/musicdl)（kwqq）接进
Songloft（原 MiMusic），提供**六源聚合搜索、320k 播放、歌词与封面提供者**。

对应 musicdl 生态路线图 **P2**；设计决策与验收记录见 `D:\_work\dev\docs\projects\MUSICDL\` 下文档。

## 能力

| 端点 | 说明 |
|---|---|
| `POST /api/search` | 六源（kuwo/qq/qianqian/migu/netease/kugou）聚合搜索，轮询交错合并，每源 4s 超时 |
| `POST /api/music/url` | 按 `source_data {s,i,c}` 解析播放直链（`quality=320k` 封顶；migu 自动回传 `copyright`）；宿主下发 fallback 时按 标题+歌手 回搜换源 |
| `GET /lyric-search` | 宿主歌词提供者（按源顺序回搜取 LRC，24h 缓存；qianqian 不参与——by-id 不支持歌词） |
| `GET /cover-search` | 宿主封面提供者 |
| `POST /import` | 插件 UI 专用：把搜索结果写进宿主曲库（`/api/v1/songs/remote`，`dedup_key=kwqq_<源>_<id>` 去重） |
| `GET/POST /config` | 设置（kwqq-api 地址 / API key / 启用源 / 音质封顶），storage 持久化 |

插件 UI（`static/index.html`）：搜索页（点结果 → 导入曲库 → `player.setQueue` 走宿主播放器，可直接 DLNA 投送）+ 设置页。

## 安装（手机 Songloft）

1. 取 `dist/kwqq.jsplugin.zip`（构建产物，见下）。
2. Songloft → 设置 → 插件 → 上传 `.jsplugin.zip`（或经插件商店/registry 分发，后续）。
3. 插件页 → 设置 → 填 kwqq-api 地址（内网 `http://10.10.10.2:3003` 免 key；外网/WG 填 key）。
4. 搜索页搜歌 → 点击播放。歌词/封面由宿主在播放时向本插件拉取。

### 验收判据（对应 roadmap P2）

- [ ] 手机插件页搜「晴天」→ 出六源结果、首条 酷我/周杰伦
- [ ] 点击后宿主播放器出声（或先回宿主歌曲页播放）
- [ ] 投送 LX06（DLNA）出声、暂停/恢复/拖进度正常
- [ ] 歌词显示且随播放滚动（宿主播放器路径，与音流不同）
- [ ] 重复点击同一首不产生重复条目（dedup 生效）

## 服务端已验证项（2026-10-28，评估实例 = 家庭 PC WSL）

- 插件挂载 + lyric/cover provider 注册 ✅
- search：36 条 / 6 源全 ok / UTF-8 正常 ✅
- music/url：kuwo 320k mp3（Range 206 + audio/mpeg 实测）、QQ 链返回 flac（kwqq 已知行为，如实透传，宿主 ffmpeg 可转码）✅
- import：宿主 song id=3 返回；二次导入 dedup 同 id ✅
- **闭环**：`GET /api/v1/songs/3/play` → 200 audio/mpeg 10.8MB（宿主经插件解析并代理拉流）✅

## 构建

```bash
npm install            # node 20+；esbuild postinstall 需 npm install-scripts approve esbuild（npm 11 默认拦）
npx songloft-plugin build   # → dist/kwqq.jsplugin.zip（main.js 编译为 QuickJS 字节码）
npx songloft-plugin validate
```

源码为纯 JS（ES2020），无 TS 构建链；`@songloft/plugin-builder` 负责 static hash、入口编译与打包。

## 实现注意（踩坑记录）

- **宿主 QuickJS 没有 `AbortController`/`AbortSignal.timeout`** —— 超时用 `Promise.race`（输掉的 fetch 由宿主 30s 默认超时回收）。
- 宿主全局 `fetch` 免权限；插件 HTTP 路由默认带 JWT，`publicPaths` 未声明（无音箱直连需求，宿主代理播放）。
- 插件 UI 鉴权：宿主把 access_token 注入 `localStorage['songloft-auth']`；本插件路由请求需带 `Authorization: Bearer`。
- 播放走宿主播放器（`player.setQueue`）而非插件页内 `<audio>`：保住宿主的投送按钮/队列/歌词/锁屏控制。go-music-dl 选择页内播放是为了桥接其外部实例；kwqq 无此约束。
- QQ 链在 `quality=320k` 下仍可能返回 flac（kwqq-api 台账 documented 行为），插件如实透传。

## 关联

- 上游 kwqq：`D:\_work\repos\musicdl`（`server/`、`docs/API-REFERENCE.md`、`docs/MUSIC-SOURCES-REGISTRY.md`）
- 路线图：`D:\_work\dev\docs\projects\MUSICDL\music-ecosystem-roadmap.md`（Track P）
