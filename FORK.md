# Fork 定制说明（jndavidz/songloft）

本 fork 在上游 songloft-org/songloft 之上的全部差异，按主题索引。上游同步：fetch upstream → merge main → 跑本文各节的验收。

## 1. NAS 兼容构建（核心动机）

官方发布镜像的二进制经 **UPX 打包**，UPX 解压壳在 DSM 6.2 / kernel 3.10（群晖 DS416play）上必现
`Segmentation fault (core dumped)`（rc=139）。本 fork 的构建**不打 UPX**，其余参数与官方一致
（CGO_ENABLED=0、GOAMD64=v1、`-s -w` ldflags）。

- `Dockerfile.nas` — alpine3.20 + ffmpeg + 未加壳二进制（`-tags lite`，无 Web 前端）
- `.github/workflows/fork-image.yml` — push main 自动构建并推 `ghcr.io/jndavidz/songloft-fork:latest`
  （含「二进制不得含 UPX」自检）
- NAS 部署编排：`deploy/nas/docker-compose.yml`（host 网络 :58091，曲库/数据卷，非 root 运行）

构建命令（本地 WSL）：

```bash
git submodule update --init --recursive
VERSION=$(git describe --tags --always) COMMIT=$(git rev-parse --short HEAD) \
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOAMD64=v1 \
  go build -tags lite -ldflags "-s -w -X songloft/internal/version.Version=$VERSION ..." \
  -o bin/songloft-linux-amd64 .
docker build -f Dockerfile.nas -t songloft-fork:latest .
```

## 2. 遥测天然关闭

Tracely 上报由**构建期 ldflags 注入**（`internal/tracelycfg`，`AppID/AppSecret/Host` 三者非空才启用）。
上游官方镜像由其私有 CI 注入；本 fork 一律不注入 → `tracelycfg.Enabled()` 恒 false，客户端不初始化，
无需任何运行时开关。见 PRIVACY.md 的口径。

## 3. kwqq 音源插件（`plugins/src/kwqq/`）

自建 [kw-qq-music-api](https://github.com/jndavidz/musicdl)（六源聚合解析服务）的 Songloft 音源插件：
六源聚合搜索、320k 直链、歌词/封面 provider、导入宿主曲库后走宿主播放器（保住 DLNA 投送/队列/歌词）。
契约、构建、验收判据见 `plugins/src/kwqq/README.md`。

## 4. 与上游的联动注意

- **升级上游**：merge upstream/main 后，重点回归 ① NAS 兼容（二进制不得被新构建流程加壳）②
  插件宿主 API 契约（`/api/v1/songs/remote`、`/api/music/url`、lyric/cover provider）③ DSM 老内核
  依赖（若上游引入新 syscall）。
- **Web 前端**：`-tags lite` 不嵌 `clients/player-build/web-embedded`（需 `make build-frontend-web-embedded`，
  Lynx/rspeedy 工具链）；嵌入后即为 full 版。生产已部署 **full 版（含 Web 管理 UI）**，lite 仅用于 CI 产物/备用。
- 本地插件开发：`plugins/src/kwqq` 内 `npm install && npx songloft-plugin build` → 上传 `.jsplugin.zip`。

## 5. 部署与生态集成现状

NAS 生产部署（host 网络 :58091、full 版、kwqq 插件安装）、与 musicdl（kwqq-api）和 open-xiaoai（bridge/LX06）的交互、队列归属与缺口，详见：
[`docs/plan/songloft-deployment-and-integration-20261028.md`](docs/plan/songloft-deployment-and-integration-20261028.md)。
