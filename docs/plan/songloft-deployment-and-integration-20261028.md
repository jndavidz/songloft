# Songloft 部署现状与生态集成调查（2026-10-28）

> 范围：`D:\_work\repos\songloft`（jndavidz/songloft fork）代码与 NAS 实际部署；与 `musicdl`（kwqq-api）和 `open-xiaoai`（bridge + LX06）的交互关系；三方整合的能力面、支持面与部署规划。
> 方法：只读代码检索与文档核对 + NAS `docker inspect`/日志 + HTTP 版本探活；本调查未修改设备、未改容器配置。
> 基线（核对时点 2026-10-28）：Songloft 仓库 HEAD `ba72acb`（`v2.13.6-28-gba72acb`）；NAS 运行版本 `v2.13.6-22-ga6065a9`（full，构建于 2026-10-27 18:23:53）；musicdl `api-server` HEAD `9145d6c`；open-xiaoai HEAD `5effc3a`。

---

## 0. 结论摘要

1. **Songloft 已经是生产形态**：NAS 常驻 `songloft-fork:full`（host 网络 :58091，healthy），含 Web 管理 UI；kwqq 音源插件已安装（`jsplugins/kwqq.jsplugin.zip`），六源搜索与宿主播放闭环在服务端验证通过。
2. **媒体流由 NAS 提供，不是手机中转**：投送时下发给 LX06 的 URL 是 `http://10.10.10.2:58091/api/v1/songs/{id}/play?access_token=...`；当前曲的播放不依赖手机保持前台。
3. **但播放队列在手机 App 里，不在服务端**：`PlayQueue` 是 Flutter 客户端状态，投送完成监听（`_onDeviceCompleted`）由 App 推进下一首。手机被杀/深度休眠会影响"自动续播"，bridge 也无法凭服务端 API 做 App 会话的语音 next/prev。
4. **与 musicdl 是单向依赖**：插件通过 HTTP 调 kwqq-api 的 `/search`、`/song/url`、`/lyric`、`/album`；音量、歌词、专辑、换源都在插件内适配。kwqq-api 挂掉，在线源失效但本地曲库不受影响。
5. **与 bridge 的共同作用点是 DLNA 渲染器 LX06**：bridge 能读到 Songloft 投送产生的播放状态（`player_get_context.status` 1/2/0），并能用 `mphelper pause/play` 暂停/恢复；bridge 在 2026-10-28 新增的 `playback_next/prev` 只作用于**设备自有队列**、DLNA 投送中无效且真机未验，Songloft 队列又在 App 侧，语音切歌仍是架构缺口。
6. **Songloft 已内置两处对"小爱类设备"的兼容**：URL `access_token` 鉴权，以及把小爱固件把 `&` 替换成空格后的参数还原逻辑；`/songs/{id}/play?seek=N` 还能为不支持 Range 的音箱用 ffmpeg 从第 N 秒产流。这些是未来 bridge 复用的现成能力。
7. **能力面足够充当生态的"稳定流 + 曲库 + 管理"层**：服务端共注册 170 条路由，覆盖媒体流/转码、扫描入库、歌单标签、播放历史、JS 插件、主题、缓存、在线升级与歌单导入导出；但**没有服务端播放队列、没有 DLNA/Subsonic 服务端、没有事件推送**，这三条决定了整合只能在"被拉流 + 轮询"的形态内设计。
8. **对 bridge 的最高价值是稳定播放 URL 与 `seek/format/normalize`**：可替代 kwqq 裸直链的 10 分钟 TTL 与无 seek 限制；对 musicdl 的价值是现成的宿主消费端（kwqq 插件）、宿主侧缓存代理与插件分发渠道。
9. **整合路线不需要改架构**：先对齐版本（P0），再把 bridge 的语音播放源切到 Songloft URL（P1），随后收口队列语义、TTS 与音量归属（P2/P3），最后处理曲库收敛与运维（P4/P5）。
10. **明确不做**：Songloft 当语音中心（open-xiaoai 已裁决 bridge 是唯一语音中心）、Songloft 服务端队列/播放会话（除非"跨设备续播"升级为硬需求）、bilibili 投送语音点播（fMP4 + 无 headers 通道）。

---

## 1. Songloft 能力全景

### 1.1 仓库组成

| 目录 | 角色 |
|---|---|
| `cmd/`、`main.go` | Go 服务端入口 |
| `internal/app/routers.go` | HTTP 路由与中间件装配（`/api/v1`，共 170 条注册） |
| `internal/handlers/` | 业务处理：`music.go`（曲库/播放流）、`jsplugin.go`、`scan.go`、`playlist.go`、`auth.go`、`proxy.go` 等 |
| `internal/services/` | 领域服务：扫描、播放、认证、HLS、播放历史等 |
| `internal/database/` | SQLite（含 WAL）与迁移 |
| `internal/jsplugin/` | JS 插件宿主：QuickJS 桥、插件 HTTP 路由、provider、存储 |
| `internal/jsruntime/` | QuickJS 运行时封装（`modernc.org/quickjs`）、fetch polyfill、异步任务 |
| `internal/middleware`、`internal/httputil` | JWT 与小爱兼容；代理感知的 HTTP 客户端 |
| `internal/tracelycfg/` | 遥测配置（构建期 ldflags 注入） |
| `clients/player/`、`clients/player-lynx/`、`clients/tv/` | Flutter / Lynx / Android TV 客户端（submodule） |
| `plugins/src/` | 官方与自建插件（本 fork 的 kwqq 在 `plugins/src/kwqq/`） |
| `plugins/toolchain/` | 插件开发工具链（SDK / Builder / 脚手架，submodule） |
| `integrations/home-assistant/` | HA 加载项（submodule，独立仓库） |
| `deploy/nas/` | NAS 部署编排（host 网络、卷、健康检查） |
| `Dockerfile.nas`、`.github/workflows/fork-image.yml` | fork 定制镜像与 CI |

### 1.2 服务端关键机制

**鉴权**：JWT。`internal/services/auth_service.go` 生成 access token（7 天）与 refresh token（30 天）。媒体类请求（`<img>`、音频流）无法自定义 Header，`internal/middleware/auth.go` 额外接受 query 参数 `access_token`。

对小爱音箱有专门兼容：小爱固件会把 URL 中的 `&` 替换为空格，导致 `access_token` 之后的参数被吞进 token 字符串；中间件检测到 token 中含空格时，按空格拆分并把剩余 `k=v` 还原回 query（`internal/middleware/auth.go` 约 L69–80）。这条逻辑说明上游已经把"小爱直接拉取 Songloft 流"当作真实场景设计过。

**播放流**：`GET|HEAD /api/v1/songs/{id}/play`（`internal/handlers/music.go`，路由见 `internal/app/routers.go` 约 L308）。按歌曲类型分发：

- 本地文件：`http.ServeFile`，天然支持 Range/206；
- 远程歌曲（插件导入的 `remote`）：服务端代理/缓存后出流，缓存文件支持 Range；
- 电台：HLS 代理或 302（`/songs/{id}/play.m3u8`、`/songs/{id}/hls/*`）；
- 转码参数：`format=`（如 wma→mp3）、`quality=`、`track=`（多音轨）、`normalize=1`（EBU R128）、`seek=N`（ffmpeg 从第 N 秒起播的 chunked MP3，无 Content-Length/不可 Range）、`speed=`（0.5–2.0 atempo）、`media=video`；
- `/api/v1/proxy/transcode`：服务端拉远程音频 → ffmpeg 实时转 MP3（用于"不导入"场景下音箱不可解码的直链）；HLS 转码会话端点同理；转码与 seek/均衡共 4 个槽位，满则 503。

**插件宿主**：QuickJS（`internal/jsruntime`），插件声明 `onInit/onHTTPRequest`，宿主把 `/api/v1/jsplugin/{name}/...` 的请求转给插件；插件路由默认需要宿主 JWT，`publicPaths` 可声明免鉴权路径（kwqq 插件未声明，因为播放走宿主代理，不需要音箱直连插件）。插件可通过 `songloft.storage` 持久化配置，注册 lyrics/cover provider，并通过 `player.setQueue` 操作宿主播放器队列。

**数据库**：SQLite（`/app/data/songloft.db`，WAL 模式），含曲库、歌单、插件、播放历史等表；迁移在 `internal/database/migrations`。

**缓存与代理**：音乐缓存为流式旁路缓存（LRU，独立目录与容量上限）；资源代理含 CORS 与 SSRF 防护、私网白名单，外发请求走代理感知客户端。

### 1.3 API 面（170 条路由，按域归类；下表路径相对 `/api/v1`）

| 域 | 代表端点 |
|---|---|
| 媒体流与转码 | `/songs/{id}/play`、`play.m3u8`、`hls/*`、`video-hls/*`、`/proxy`、`/proxy/transcode`、`transcode/hls/sessions/*` |
| 曲库 | `/songs`（`keyword`/`type`/`path_prefix`/`sort`/`order`）、`/songs/ids`、`random`、`facets`、`folders`、`stats`、`names`、`duplicates`、`{id}`、`artists`、`tracks`、`audio-tracks` |
| 采集与整理 | `/scan`、`scan/progress`、`scan/cancel`、`scan/directories`、`scan/fingerprints/*`、`songs/organize`、`songs/refresh-metadata`、`songs/batch-delete`、`songs/clean` |
| 歌单与标签 | `/playlists` 全套（含 `songs/reorder`、`sort`、`move`、`cover`、`visibility`、`pin`、`export/import`）、`/song-tags`、`/songs/{id}/song-tags` |
| 远程曲与电台 | `/songs/remote`（插件导入）、`/songs/radio`、`/songs/{id}/activate`、`/songs/{id}/played` |
| 历史 | `/play-history`（按播放上下文，50 条裁剪） |
| 插件 | `/jsplugin/{entry}` 宿主路由、`/settings/plugin-registries`、`plugin-keep-alive`、`plugin-auto-update`、`plugin-order` |
| 主题 | `/theme-packs`、`theme-packs/catalog/*`、`theme-packs/active` |
| 设置 | `/settings/*`（24 组强类型开关：扫描、HLS、代理、均衡、曲库浏览、标签回写等）、`/configs/*`（admin 后门） |
| 运维 | `/cache-manage/*`、`/logs/export`、`/upgrade/*`（Docker 在线升级/回退）、`/health`、`/version` |
| 认证 | `/auth/refresh`、`/auth/logout`、`/auth/tokens` |

### 1.4 客户端矩阵与投送

| 客户端 | 平台 | 说明 |
|---|---|---|
| Flutter | Android/iOS/macOS/Windows/Linux/Web | 全功能，支持 Bundle 本地模式与 **DLNA 投送** |
| ReactLynx | Android/iOS/HarmonyOS/Web 预览 | 无 Bundle、无 DLNA、无单曲离线缓存 |
| Songloft Desktop（Electron） | Windows/macOS/Linux | 需独立服务器 |
| Songloft TV（Kotlin） | Android TV / 大屏 | 遥控器、K 歌、扫码点歌；部分能力依赖插件 |

投送实现：`clients/player/lib/features/dlna/data/dlna_service.dart`，基于 `dlna_dart` 做 SSDP 发现与 SOAP 控制（`SetAVTransportURI`/`Play`/`Pause`/`Seek`/`SetVolume`），并轮询 `GetPositionInfo`。

投送 URL：`UrlHelper.buildResourceUrl` 把相对路径拼成 `resolvedBaseUrl + /api/v1/... + ?access_token=...`；`_toCastReachableUrl` 只在 `127.0.0.1/localhost` 时改写为 LAN 地址（Bundle 本地模式），远程服务器场景保持 `10.10.10.2`。因此：**投送时 LX06 拉的是 NAS 的流**，不是手机本地服务。

队列：`clients/player/lib/features/player/domain/use_cases/play_queue.dart` 的 `PlayQueue`（不可变值对象），播放状态/队列快照持久化在客户端（`playback_state_storage.dart`）。服务端没有"播放队列"表或 API。

投送续播：`dlna_provider.dart` 的 `_onDeviceCompleted()` 在设备端曲目结束时按 order/loop/random 推进队列，再 `castSong(next)` 把下一首 URL 发给设备。**自动续播依赖 App 活着**。

歌词：本地播放时由播放器进度驱动；投送时本地播放器被 `pauseLocal()` 暂停，歌词进度源冻结，所以"投送时歌词不滚动"（根因与修复方案见 `docs/plan/dlna-lyrics-follow.md`）。

### 1.5 fork 定制

| 定制 | 位置 | 作用 |
|---|---|---|
| 无 UPX 构建 | `Dockerfile.nas`、CI `fork-image.yml` | 官方镜像的 UPX 壳在 DSM 6.2/kernel 3.10 上段错误；fork 直接跑通 |
| 遥测天然关闭 | `internal/tracelycfg` + 构建参数 | AppID/Secret/Host 不注入即不初始化，无运行时开关 |
| full 变体 | `Dockerfile.nas` 的 `SONGLOFT_BIN`、`deploy/nas` | 嵌入 Lynx Web 前端，产出 Web 管理 UI（lite 仅 API/App） |
| CI | `.github/workflows/fork-image.yml` | push main 构建 lite 镜像推 ghcr，并含"二进制不得含 UPX"自检 |
| kwqq 插件 | `plugins/src/kwqq/` | 六源搜索/直链/歌词/封面/专辑 + 导入宿主；`minHostVersion 2.9.0`、权限 `storage,net` |

### 1.6 能力边界（决定整合形态）

1. **无服务端播放会话/队列**：`PlayQueue` 在客户端，服务端只有播放历史，没有 next/prev API。
2. **无 DLNA/UPnP/Subsonic 服务端**：只能被自家客户端和"拉到 URL 就能播"的设备消费（音箱、浏览器、播放器）；Lyrion 等只能通过共享目录读文件。
3. **无事件推送/webhook**：外部系统只能轮询 `/api/v1/...` 或设备侧状态。
4. **转码算力有限**：NAS 为 Celeron N3060，常态转码需限量（4 槽位），语音/投送应优先直出 320k mp3 或原文件。
5. 上游单人主导、发版频繁；升级必须回归插件契约（`/songs/remote`、`/api/music/url`、lyric/cover provider）。

---

## 2. NAS 部署现状（实测）

### 2.1 主机与容器

- 主机：群晖 DS416play，DSM 6.2 / kernel 3.10.108 / Celeron N3060（老内核是 UPX 段错误的根因）。
- 容器：`songloft`，镜像 `songloft-fork:full`，`network_mode: host`，`user 1028:100`（非 root，避免 Synology ACL 写出的文件权限为 `000`），`restart: unless-stopped`。
- 状态：`Up 10 hours (healthy)`；`http://10.10.10.2:58091/` 返回 200（Web 管理 UI，HTML 约 31KB）。
- 健康检查：`wget -qO- http://127.0.0.1:58091/`，60s 间隔；日志里能看到每分钟一次 `GET / 200`。

### 2.2 版本与镜像差异

- 运行版本：`v2.13.6-22-ga6065a9`，`build_type=full`，构建时间 2026-10-27 18:23:53。
- 仓库 HEAD：`v2.13.6-28-gba72acb`。两者差 6 个 commit：插件 v2（专辑浏览）、full 变体支持、compose 对齐、CI 清理、gitignore/https、plan 文档。**服务端功能差异很小**（插件是独立 zip，full 变体已在镜像里），但"运行镜像落后于仓库"应在下次重建时对齐。
- CI 产物是 **lite** 镜像（`ghcr.io/jndavidz/songloft-fork:latest`，无 Web 前端）；生产用的是**本地构建的 full 镜像**（`songloft-fork:full`）。二者不要混用。

### 2.3 卷与数据

| 宿主路径 | 容器路径 | 内容 |
|---|---|---|
| `/volume1/music` | `/app/music` | 曲库（与 SMB 共享 `\\10.10.10.2\music`、Lyrion 共用） |
| `/volume2/docker/songloft/data` | `/app/data` | `songloft.db`（+wal/shm）、`jsplugins/`、`jsplugins_data/`、`covers/`、`cover_thumbs/`、`music_cache/`、`logs/` |

插件实测：`jsplugins/kwqq.jsplugin.zip`（20KB，2026-10-28 02:39），`jsplugins_data/kwqq` 已生成 —— 说明插件已安装并运行过。备份时应连同 `jsplugins_data` 与数据库一起备份（WAL 模式下建议停容器或用 SQLite 在线备份）。

### 2.4 配置与鉴权

- 环境变量：`LISTEN_PORT=58091`、`DB_PATH=/app/data/songloft.db`、`MUSIC_DIR=/app/music`、`ADMIN_USERNAME=admin`、`ADMIN_PASSWORD`（见 NAS compose，勿写入文档）、`TZ=Asia/Shanghai`。
- 仅 LAN：`http://10.10.10.2:58091`，无 TLS；未做公网映射。管理口令 + access token 都走明文 HTTP，安全边界等于家庭 LAN。
- access token：7 天；refresh token 30 天；媒体 URL 里会带 `access_token`，注意日志/截图脱敏。

### 2.5 运行观察

- 日志（最近 10 小时）：只有健康检查的 `GET /`，没有 Tracely 上报错误/请求 —— fork 关闭遥测在运行态得到验证。
- 未观察到崩溃、重启、OOM；容器 healthy。

### 2.6 运维要点

- **升级**：merge upstream → 重新构建无 UPX 二进制（lite/full 按需要）→ 跑插件回归（六源搜索、直链、导入去重、投送）→ 更新容器。
- **回滚**：保留上一版镜像 tag（`songloft-fork:previous` 之类）；数据卷向后兼容由上游迁移保证，降级需谨慎。
- **备份**：`/volume2/docker/songloft/data` 全量 + `/volume1/music` 的元数据（曲库本身另有备份策略）。
- **监控**：容器 healthcheck + `/api/v1/health`；日志文件 `logs/` 可导出。

---

## 3. 与 musicdl（kwqq-api）的交互

### 3.1 拓扑

```
手机 Songloft App ──HTTP/JWT──> Songloft :58091（NAS）
        │                          │
        │ DLNA 控制                 ├─ 插件 QuickJS ──HTTP──> kwqq-api :3003（NAS）──> 各平台
        ▼                          │                              ▲
   LX06 :9999（渲染器）            └─ 曲库 /volume1/music ←── webgui :3004（按需）
        ▲
        └── bridge :9092（观察/控制）
```

### 3.2 插件对 kwqq 的调用契约

| 插件路由 | kwqq 端点 | 用途 |
|---|---|---|
| `POST /api/search` | `GET /{source}/search?keywords=&limit=` | 六源聚合搜索，轮询交错合并，每源 4s 超时、整体约 5s 预算 |
| `POST /api/music/url` | `GET /{source}/song/url?id=&quality=320k` | 直链解析；migu 自动回传 `copyright`；失败时按"标题+歌手"回搜换源 |
| `GET /lyric-search` | `GET /{source}/search` + `GET /{source}/lyric?id=` | 宿主歌词 provider；24h 缓存；跳过 qianqian（by-id 无歌词） |
| `GET /cover-search` | `GET /{source}/search`（取 `cover`） | 宿主封面 provider；24h 缓存 |
| `GET /album` | `GET /{source}/album?name=&artist=` | 专辑视图；netease 先行，其余源 501 时 UI 提示 |

插件默认配置：`base=http://10.10.10.2:3003`、`key` 留空（内网免 key）、六源默认、`quality=320k`。kwqq 返回 401 时插件给出"填 key 或让 API 信任当前网段"的提示。

musicdl 侧事实（2026-10-28 核对）：`api-server` 分支 HEAD `9145d6c`；FastAPI 单镜像双容器（API `:3003`、webgui `:3004`，webgui `restart:"no"` 按需拉起）；统一信封 `{code,msg,data,timestamp}`；直链缓存 10 分钟；七源 kuwo/qq/qianqian/migu/netease/kugou/bilibili；未完成项见其 `docs/plan/OPEN-ITEMS.md`（kuwo/QQ 专辑、kugou 歌单、上游同步）。

### 3.3 鉴权与网络

- kwqq-api 的内网免 key 网段默认含 `10.10.10.0/24`，Songloft 容器 host 网络，源 IP 即 NAS 的 LAN IP，因此插件无需 key。
- 若把 Songloft 或 kwqq 移到其他网段，需要给插件配 key，或在 kwqq 侧加可信网段。
- kwqq 是**唯一解析层**：Songloft 不直连各平台，所有上游风控/签名/换源都在 kwqq 侧处理。

### 3.4 音质与格式

- 插件显式请求 `quality=320k`，符合"播放类消费方 ≤320k mp3"的铁律。
- 已知例外：QQ 在 320k 请求下仍可能返回 FLAC（kwqq 台账行为），插件如实透传；宿主 ffmpeg/播放器可处理。
- migu 需要 `copyright` 回传；qianqian 逐曲不稳定（404 时宿主按标题+歌手回搜换源）；bilibili 默认关闭（App 内可试听，DLNA 投送不可用，m4s/remux 问题）。

### 3.5 导入宿主与去重

`POST /api/v1/songs/remote` 把插件搜索结果写入宿主曲库（`type=remote`），关键字段：`title/artist/album/duration`、`cover_url`、`source_data`（插件私有：`{s,i,c}`）、`plugin_entry_path='kwqq'`、`dedup_key='kwqq_<source>_<id>'`。宿主分配数字 id，之后 `/songs/{id}/play` 时再回调插件 `POST /api/music/url` 解析直链并代理拉流。

- 去重只在 kwqq 的 `dedup_key` 维度生效；同名的**本地文件**与**在线 remote 条目**仍可能共存（Songloft 本地扫描与插件导入是两套来源）。产品上需要在 UI 层接受这一点，或在扫描/导入侧做跨源去重（未实现）。
- 插件 UI 走"导入 + `player.setQueue`"，而不是页面内 `<audio>` 播放，目的是保住宿主的投送按钮、队列、锁屏、歌词能力——这是 Songloft 作为播放中枢能成立的关键工程选择。

### 3.6 与 webgui 的关系

webgui（:3004，按需拉起）把下载文件写入 `/volume1/music`，Songloft 扫描同一目录后把本地无损曲目纳入曲库；Lyrion 也读同一目录。三者通过"共享目录"解耦，没有 API 耦合。注意 webgui 容器写文件时曾在 Synology ACL 下产生 `000` 权限，已通过落盘后 `chmod 644` 修复；Songloft 以 1028:100 运行，必须能读到这些文件。

### 3.7 失败模式

| 故障 | 表现 | 兜底 |
|---|---|---|
| kwqq-api 不可达 | 搜索/在线播放失败 | 本地曲库不受影响；插件超时后返回空/错误 |
| 某源超时/风控 | 该源无结果 | 其余源继续；搜索 4s/源、整体约 5s |
| 直链 404/403 | 单曲播放失败 | 宿主按标题+歌手回搜换源；qianqian 常见 |
| 专辑源不支持 | `/album` 501 | 插件 UI 提示"该源不支持专辑检索" |

### 3.8 宿主能给 musicdl 的支持

1. **现成消费端**：kwqq 插件（`2026.10.28`，`minHostVersion 2.9.0`，权限 `storage,net`）已是生产形态的消费者，接口契约稳定。
2. **把 10 分钟直链风险挡在宿主侧**：remote 条目导入后，`/songs/{id}/play` 由宿主重新解析并代理拉流+缓存，音箱/客户端拿到的是可长期复用的宿主 URL。
3. **插件分发渠道**：插件商店/注册表/自动更新可把 kwqq 作为自建源上架，版本随插件走，无需手动传 zip。
4. **共享目录解耦**：webgui 下载 → `/volume1/music` → Songloft 扫描 → Lyrion 同读，无 API 耦合。
5. **注意**：musicdl `docs/MUSIC-SOURCES-REGISTRY.md:298` 含明文管理口令，整合前应脱敏；kuwo/QQ 专辑与 kugou 歌单仍开口（musicdl 侧 P1）。

---

## 4. 与 open-xiaoai-bridge 的交互

### 4.1 当前语音点歌链路（与 Songloft 无关）

bridge（NAS :9092）的语音点歌目前走：bridge → kwqq-api 解析 → `/api/play/url` 把**裸直链**交给 LX06 `player_play_url`。这条链的约束：
- `/api/play/url` 只接受 `url/blocking/timeout`，不能带 headers、没有队列/seek；
- kwqq 直链有 10 分钟服务端缓存 TTL，长播/续播存在断流风险；
- bilibili 不可用（m4s）。

Songloft 不在这条链上，**两者目前是并行而非串联**。

### 4.2 Songloft 投送链路与 bridge 的可见性

Songloft App 作为 DLNA 控制点，把 NAS 流 URL 发给 LX06。实测事实（2026-10-27/28，DLNA 通用路径）：

| 项 | 结果 |
|---|---|
| bridge 可见性 | LX06 `mediaplayer player_get_context.status` = 1 播放 / 2 暂停 / 0 空闲；`ubus listen` 有 `play_status_broadcast_event 0/1` |
| bridge 控制 | `mphelper pause` → status 2 且位置冻结；`mphelper play` 恢复 PLAYING |
| DLNA 控制点 | SetAVTransportURI/Play/Pause/Seek/Stop 全可用；Seek 触发 Range 请求 |
| 音量 | RenderingControl 与 bridge 的 `player_set_continuous_volume` 都作用于同一设备音量，暂未发现重播（#516 类问题未复现） |

结论：**bridge 可以把 Songloft 投送当作普通 DLNA 播放来观察和暂停/恢复**；`casting_pause {from:"tts"}` / `casting_resume` 的 TTS 协调语义对该路径同样适用（尚未在 Songloft 投送进行中做 TTS 打断实测）。

### 4.3 队列与切歌：缺口与 2026-10-28 的进展

- Songloft 队列在 App（`PlayQueue`），投送完成由 App 的 `_onDeviceCompleted` 推进下一首。
- bridge 在 2026-10-28 为 `song_request` 增加了 `action=album`（按官方曲序组队列）与能力 `playback_next/prev`（`mphelper iot_next/iot_prev`）；但能力说明明确"仅设备自有队列有效，**DLNA 投送中无效**"，且真机未验（`bridge/core/device/registry.py:512-543`）。
- 因此：
  - 由 **bridge 自己发起的播放**（`/api/play/url` + 技能内队列）可以做 next/prev；
  - 由 **Songloft App 发起的 DLNA 投送**，bridge 只看到"设备在播什么"，**语音切歌仍无解**（队列在 App，bridge 不掌握下一首）；
  - 手机被杀、深度休眠或 App 被系统回收时，当前曲可能继续播（NAS 供流），但**不会自动切下一首**；
  - 若"App 投送态的语音切歌"是硬需求，需要 Songloft 新增服务端播放会话/队列 API 让 App 与 bridge 共享队列，或引入独立编排器。这是一次架构级改动，不是插件层能解决的。

### 4.4 可复用的 Songloft 能力（给 bridge 的备选集成）

1. **稳定播放 URL + 7 天 token**：`/api/v1/songs/{id}/play?access_token=...` 比 kwqq 的 10 分钟直链稳定；中间件已兼容小爱把 `&` 换成空格的问题。
2. **`seek=N` 产流**：`/songs/{id}/play?seek=120&format=mp3` 用 ffmpeg 从第 120 秒产出 chunked MP3，专为不支持 Range 的音箱设计（代码注释明确提到"小爱经 player_play_url 只会从头拉流"）。这正好补上 bridge `/api/play/url` 没有 seek 的缺口。
3. **`format=mp3` / `/proxy/transcode`**：对不可解码的源（wma、部分远程容器）强制转 MP3；bilibili 的 fMP4 仍不可投送。
4. **本地曲库检索**：`GET /api/v1/songs?keyword=`（`internal/handlers/music.go:385`）可直接命中本地文件，是"本地/无损模式"的一条不依赖 Lyrion 的路径（LX06 的 libVLC 能解码 flac 时成立；DSD 需转码）。
5. **插件六源搜索**：bridge 可带宿主 JWT 调 `/api/v1/jsplugin/kwqq/api/search`，与 App 共用同一套源策略。
6. **`media=video` / HLS 端点**：视频/电台场景可复用（当前语音场景不需要）。
7. **播放事件**：Songloft 服务端有播放历史与 `songs/{id}/played`，但 bridge 目前只读设备侧状态，不消费 Songloft 事件。

### 4.5 潜在集成方案

| 方案 | 做法 | 收益 | 代价/风险 |
|---|---|---|---|
| A. 语音播放改用 Songloft URL | bridge 通过 Songloft API 找 song id（或由 kwqq 结果映射到已导入条目），下发 `/api/v1/songs/{id}/play?format=mp3&seek=0&access_token=…` | 稳定 URL、服务端缓存/转码、可 seek | 需要给 bridge 发 token 并处理续期；要维护 kwqq→Songloft id 映射；多一跳 |
| B. 服务端队列 API | fork 新增"播放会话/队列/next/prev"端点，App 与 bridge 共用 | App 投送态也能语音切歌、跨设备续播 | 架构改动大，需改客户端；与"bridge 是唯一语音中心"的既有裁决冲突，暂缓 |
| C. TTS 协调 | 沿用 `casting_pause/resume`，在 Songloft 投送态实测并补超时 | 不打断音乐 | 需真机验证；与 App 的投送状态机可能有竞态 |
| D. 音量归属 | 明确"谁拥有音量"：bridge 连续音量 vs App RenderingControl | 避免双控制 | 需要约定 + 实测 |

> 计划采用 **A（P1）+ C/D（P3）**；B 仅在"App 投送态语音切歌"成为硬需求时重新评估。

### 4.6 安全与耦合

- Songloft 的 access_token 会出现在媒体 URL 中，若 bridge 日志打印完整 URL 会泄露；建议 bridge 侧脱敏。
- Songloft 与 bridge 都依赖 LX06 的 DLNA 面（9999 无鉴权）；家庭 LAN 内任何设备都可控制，这是既有事实。
- 两者不共享队列/状态数据库：bridge 只知道"设备在播什么"，不知道"App 的下一首是什么"。这是所有"手机 App 队列"方案的共同限制。
- 既有裁决（open-xiaoai `docs/plan/music-ecosystem-voice-plan.md` §0）：**Songloft 服务端已从语音计划中移除，bridge 是唯一语音中心**；Songloft 只作为"手机投送（补充目标）"进入该计划。本文件的规划不改变这一裁决。

---

## 5. 生态支持面（给另两个项目的具体帮助）

### 5.1 给 open-xiaoai（bridge）

| # | 支持项 | 落地方式 | 前提/限制 |
|---|---|---|---|
| 1 | 稳定播放 URL | `/api/v1/songs/{id}/play?access_token=…&format=mp3&seek=N&normalize=1` | token 7 天需续期；URL ≤1024 字节；无 headers 通道 |
| 2 | 本地曲库语音检索 | `GET /api/v1/songs?keyword=` → 取 id → 播放 URL | 需服务账号 JWT；DSD/不兼容格式需转码 |
| 3 | 在线源统一入口 | 同一宿主 JWT 调 kwqq 插件路由 | 插件路由不公开，必须带 token |
| 4 | 语音队列/切歌 | bridge 技能队列逐首向 Songloft 取 URL；`next/prev` 用桥队列 | 仅 bridge 发起的会话有效；App 投送态无效 |
| 5 | 可观测 | Web 管理台、播放历史、`/api/v1/health`、`/api/v1/version`、日志导出 | 无事件推送，需轮询 |

### 5.2 给 musicdl（kwqq-api）

| # | 支持项 | 落地方式 | 前提/限制 |
|---|---|---|---|
| 1 | 生产消费端 | kwqq 插件（六源搜索/直链/歌词/封面/专辑） | 契约变更需 `minHostVersion` 与回归 |
| 2 | 直链风险兜底 | remote 导入 + 宿主代理拉流/缓存 | 缓存占盘；DS416play 算力有限 |
| 3 | 插件分发 | 插件商店/注册表/自动更新上架 kwqq | 需维护 registry 条目与签名/哈希 |
| 4 | 共享目录解耦 | webgui → `/volume1/music` → 扫描 → Lyrion | 文件权限需 1028:100 可读（644） |
| 5 | 安全提醒 | — | `MUSIC-SOURCES-REGISTRY.md:298` 明文口令需脱敏 |

---

## 6. 整合部署规划

### 6.1 角色分工与目标拓扑

- **Songloft**：音乐库 + 稳定流 + 转码 + 管理台/App（不做语音中心）。
- **musicdl / kwqq-api**：目录与解析层（`:3003`，webgui `:3004` 按需）。
- **open-xiaoai bridge**：语音编排、TTS、设备控制（`:9092/9093/4399`）。
- **Lyrion + NUC Squeezelite**：高保真回放（不变）。

```
手机 App ─DLNA─┐                    ┌─ kwqq 插件 ─→ kwqq-api:3003 ─→ 各平台
              ├→ LX06:9999 ←────────┤
LX06 语音 ─→ bridge:9092 ──URL──────┴→ Songloft:58091 /songs/{id}/play?token
                    │                        │
                    └──状态轮询──────────────┘  /volume1/music（共享）
```

### 6.2 阶段计划

| 阶段 | 内容 | 验收 | 主要风险 |
|---|---|---|---|
| **P0 基线对齐** | 用 `ba72acb` 重建 `songloft-fork:full`（无 UPX），打 `songloft-fork:previous` 回滚 tag；备份 data 卷（含 `jsplugins_data`）；插件回归 | `/api/v1/version` 与镜像一致，healthy，六源/直链/导入/投送全绿 | 重建后插件契约变化；备份不完整 |
| **P1 语音播放改走 Songloft URL** | bridge 增加 Songloft 客户端：local 检索 → 播放 URL；未命中 → kwqq 搜索 → 导入 remote → Songloft 出流；不可达回落 kwqq 裸直链；服务账号 + refresh 自动续期 + 日志脱敏 | 本地/在线各点 3 次出声；断 kwqq 仍播本地；30 分钟不断流；`seek` 生效 | token 失效静默失败；多一跳延迟 |
| **P2 队列语义收口** | bridge 维护语音会话队列，逐首取 URL；`next/prev` 走桥队列，空则回落设备队列；App 投送态的缺口在文档与 UI 上明示 | 语音连播 5 首按序；"下一首/上一首"10 次成功率 ≥95% | 与设备队列状态竞态 |
| **P3 TTS 与音量** | 实测 `casting_pause/resume` 对 Songloft 投送是否幂等并补超时；定音量唯一 owner | 播报不吞歌、播完必恢复；无音量重播 | 状态机竞态；真机窗口 |
| **P4 曲库收敛** | 扫描排除下载临时目录；`1028:100` 可读；跨源去重（local vs remote）列为后续需求 | webgui 下载 → 扫描 → App 可播，无需重启 | 权限 000；重复条目 |
| **P5 运维与安全** | healthcheck + `/health` + bridge `/api/status` + musicdl `/metrics`；升级 SOP；:58091 不出公网；口令脱敏与轮换 | 一次完整升级+回滚演练通过 | 老内核依赖；上游破坏性变更 |

### 6.3 明确不做

1. Songloft 当语音中心（既有裁决：bridge 是唯一语音中心）。
2. Songloft 服务端队列/播放会话（除非"App 投送态语音切歌/跨设备续播"升级为硬需求）。
3. bilibili 的投送与语音点播（fMP4 + 无 headers 通道）。
4. master/surround51 加密件进入任何播放层（musicdl ADR-0002）。
5. 用 Songloft 替换 Lyrion 的高保真输出链路。

### 6.4 决策点与回滚

- 先做 P0 再做 P1：镜像与仓库不一致会让联调结论不可复现。
- P1 的 token 策略是唯一"必须设计对"的环节：401 自动刷新 + 健康探针 + 与 kwqq 的降级开关。
- 每个阶段保留可回滚点：镜像 tag、bridge 配置开关（`song_request.enabled`、源选择）、kwqq 插件版本。

---

## 7. 安全、合规与维护

- **许可**：上游 Apache-2.0（含 NOTICE/TERMS/PRIVACY）；fork 保持同许可，修改需保留声明。
- **遥测**：fork 构建不注入 Tracely 凭据 → 客户端不初始化；运行日志无上报错误。
- **攻击面**：:58091 仅 LAN、无 TLS；admin 口令 + 7 天 access token；插件路由带宿主 JWT；kwqq 内网免 key。不要把 :58091 映射到公网。
- **文件权限**：容器非 root（1028:100），下载/扫描文件需对该用户可读；webgui 已修 `000` 权限问题。
- **上游风险**：单人主导、发版频繁；插件依赖 `/api/v1/songs/remote` 与 `player.setQueue` 两个宿主契约，升级必须回归；fork 的 CI 与 `Dockerfile.nas` 要跟随上游构建参数变化。
- **运行镜像落后仓库 6 个 commit**：功能影响小，但应在下次变更时重建并对齐版本号。
- **凭据 hygiene**：媒体 URL 带 token（日志脱敏）；musicdl 注册表文档含明文口令；NAS compose 含 admin 口令（勿入库）。

---

## 8. 待验证与已知缺口

| # | 事项 | 状态/说明 |
|---|---|---|
| 1 | 插件真机验收：搜歌→播放→投 LX06→歌词→专辑整张连播 | **未完成**（服务端已验证，真机只验证了本地曲库投送） |
| 2 | 投送时歌词不滚动 | 根因已定位（本地播放器进度冻结），修复方案见 `docs/plan/dlna-lyrics-follow.md`，未施工 |
| 3 | 手机息屏/杀进程对"自动续播"的影响 | 预期：当前曲继续（NAS 供流），不自动切歌；未实测 |
| 4 | access token 7 天到期与长播 | 单曲不受影响；跨天连续投送需 App 刷新 token；未实测 |
| 5 | bilibili 投送 | 不可行（m4s/fMP4），已裁决接受 |
| 6 | bridge 与 Songloft 的 TTS 打断/音量归属 | 未联调 |
| 7 | 运行镜像 vs 仓库 HEAD | 差 6 commit，建议重建（P0） |
| 8 | 上游升级回归 | 需要 SOP 与插件回归清单 |
| 9 | bridge 语音播放改走 Songloft URL | 未实施（P1）；需先解决 token 续期 |
| 10 | bridge `playback_next/prev` 真机验证 | 未验证（open-xiaoai 2026-10-28 记录：仅设备自有队列、DLNA 投送中无效） |
| 11 | 跨源去重（本地 vs remote） | 未实现，仅在 kwqq `dedup_key` 维度去重 |

---

## 9. 附录：关键路径与命令

- 代码：`internal/handlers/music.go`（流）、`internal/middleware/auth.go`（token/小爱兼容）、`internal/jsplugin/`（插件宿主）、`clients/player/lib/features/dlna/`（投送）、`plugins/src/kwqq/`（插件）
- 部署：`Dockerfile.nas`、`.github/workflows/fork-image.yml`、`deploy/nas/docker-compose.yml`、`FORK.md`
- 运行探活：`curl -s http://10.10.10.2:58091/api/v1/version`
- 容器：`docker inspect songloft`（网络/卷/环境）、`docker logs --tail 100 songloft`
- 插件：`/volume2/docker/songloft/data/jsplugins/kwqq.jsplugin.zip`、`jsplugins_data/kwqq`
- 相关文档：`docs/plan/dlna-lyrics-follow.md`、`plugins/src/kwqq/README.md`、musicdl `docs/API-REFERENCE.md` / `docs/plan/OPEN-ITEMS.md`、open-xiaoai `bridge/docs/openxiaoai-voice-api.md` / `docs/plan/music-ecosystem-voice-plan.md`

---

> 第 1–4 节与第 7 节为只读调查记录（未改动 NAS 容器、设备或桥接配置）；第 5–6 节为能力面梳理与整合部署规划，尚未实施。实施时应按阶段另立验收记录，并在完成 P0/P1 后回写本文档的"待验证"表。
