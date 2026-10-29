# S-03: 实现 TrackRef 当前项准备并禁止在线预取

Status: in-progress（2026-10-29 首切片落地：managed 域核心 + 契约锁定 + 状态机；剩余切片见 Comments 末尾）
Type: task
Blocked by: 01
External blocked by: musicdl K-01/K-02（契约联调）

## Body

在 managed 路径落地 TrackRef 和当前项准备，复用 kwqq v2 身份与播放 profile。仅 Songloft 准备主体可取得标记 `requires_transcode` 的 AAC 规范化输入，AAC 不得直接 ready 或投送。搜索、候选导入、队列建立和客户端在线预取不触发直链解析；只有当前 entry 才解析、下载、必要转码和完整验证。remote 生命周期首版只覆盖准备状态与恢复，不做 GC。

### DoD

- B站 cid/page、咪咕 copyrightId 和 fallback 实际来源端到端保留；本地 song ID 与 TrackRef 不混用。
- 仅最终验证通过的 ≤320k MP3 进入 ready；原始、中间和最终表示隔离，失败不原码兜底。
- 20 项队列／导入在选中前 resolve=0，选中后仅当前项解析；显式元数据刷新等既有非 managed 行为不混淆。
- operation 查询／取消绑定 issuer 与 session/epoch/entry，迟到 ready 不可投送。

### Tests

- Go 复用音乐导入、metadata refresh、cache/transcode 测试；客户端复用 prefetch 与 queue 测试。
- 覆盖不同 cid/copyright、并发准备、取消、迟到 ready、非 MP3 和无下一项解析；AAC 正向规范化与禁止 AAC 直投均验，接入 no-prefetch/quality-profile。

### Rollback

关闭 managed prepare，保留 additive remote 状态和缓存；普通旧播放路径不变。

## Authorization

离线 fake resolver 与媒体 fixture；真实上游、客户端构建和 NAS 压测另行批准。

## Comments

### 首切片（2026-10-29）：internal/services/managed 域

现状调查结论（Explore 全扫）：Songloft 解析契约只有插件 `POST /api/music/url`（kwqq 插件 →
自建 kw-qq-music-api，非 musicdl server）；`?prefetch=1` 是唯一预取入口（客户端
PrefetchStrategy 驱动）；remote 歌除 cache_path 外无任何状态字段；source_data 是插件
opaque 身份；B站 cid/page 全仓库从未出现。v2 契约对接此前为零。

落地（全部离线可测）：

- `trackref.go`：TrackRef 契约 Go 移植，Canonical/Digest 与 musicdl `server/trackref.py`
  **逐字节一致**（golden 向量由 Python 实跑产出并锁定；含身份 extra 白名单——仅 migu
  copyrightId、bilibili cid/page，kwqq 的 c 字段按契约丢弃；缺字段/畸形明确拒绝）。
  同 BV 不同 page、同 migu id 不同 copyrightId digest 必不同（防串缓存锁测试）。
- `resolve.go`：kwqq v2 `POST /v2/playback/resolve` 客户端（X-Service-Token、envelope
  语义：HTTP 200 + code 403/404 也是失败；上游宣称 verified=true 按契约拒绝）。
  代理经 `httputil.NewClient`（pitfalls 全局代理纪律）。
- `verify.go`：最终表示帧级验证（纯 Go）——ID3v2 syncsafe 跳过、逐帧同步/帧长推进、
  每帧码率 ≤320k 上限、时长估计、尾部只认 TAG/APETAGEX/LYRICS200（宽容尾部豁免会放行
  截断流，已收紧）；ffprobe 交叉验证经 Prober 注入（codec 非 mp3 / 时长分歧即拒）。
- `download.go`：真实下载器——实际累计字节上限（不信 Content-Length）、总时间上限、
  逐跳自管重定向（强制 `CheckRedirect=ErrUseLastResponse`）、**跨源重定向丢弃全部上游
  headers**（SSRF 凭证外带防线）、HostChecker 逐跳校验（生产接 services.IsHostnameAllowed
  封私网）、失败不留部分文件、走 `httputil.NewDownloadClient`。
- `transcode.go`：真实 ffmpeg AAC→MP3 规范化（transcode 非 remux，契约 5.1 规则 9），
  包级串行信号量，产物只落 `intermediate.mp3` 中间名。
- `prepare.go`：准备操作状态机（preparing→ready/failed/canceled/interrupted）——
  operation 绑定 issuer+session/epoch/entry；换代即取消旧代次（迟到 ready 不可投送，
  `startFinish` 终态守卫防 double-close panic）；op 自带 ctx，取消/换代让真实 worker
  退出；每 op 按 `WorkDir/<digest>/` 隔离表示（并发准备不互踩）；DeclaredSize 与实际
  下载字节数对照；ResolvedSource/FallbackReason 随 op 记录。
- 测试 23 项（fixture：ffmpeg 实生成的真 MP3 128k/320k、FLAC/AAC 冒充、截断、纯文本），
  10 连跑绿；`TestPrepare_RapidGenerationChangeStress` 锁换代竞态。
- runner 新 case `songloft-managed-prepare`（ffmpeg 环境门 → 缺失 BLOCKED 不 skip），实测 PASS。

code-review 双轴（并行 sub-agent）修复：double-close panic（startFinish 守卫+压力测试）、
并发表示互踩（per-op digest 目录）、全局代理绕过（两处 client 换 httputil）、取消不达
真实 worker（op ctx）、DeclaredSize/fallback 未接线、死代码清理。

### 剩余切片（本票未完，勿标 done）

1. **SQLite 持久化**：StateStore 接口已留位，migration `0040_managed_prepare_ops` +
   sqlc 生成需 WSL 侧 `make sqlc`（本机无 sqlc）；重启恢复目前仅内存语义。
2. **source_data → TrackRef 适配**：kwqq 插件 `{s,i,c}` → MakeRef + migu copyrightId 映射；
   B站 cid/page 的选中时补全（kwqq `/song/info`）。
3. **投送层**：本包无 delivery 侧，「迟到 ready 不可投送」在投送侧的落实随 S-04 票据/lease。
4. **handler/BFF 接线与 no-prefetch 真实路径证据**：导入/建队/搜索走 managed Ref 的接线
   （现 no-prefetch 不变量在包内构造性成立），客户端 prefetch 策略属 clients 仓库。
5. **worker 生命周期实测**：取消后真实 goroutine/子进程退出断言（spec Testing Decisions
   「Assert real worker lifetime」），race 检测需 WSL（本机无 gcc）。
6. rollback 开关（关闭 managed capability）随接线切片落地。

Not run：真实上游联调（K-01/K-02 已 resolved 待联调窗口）、客户端构建、NAS。