# NAS 兼容运行镜像（jndavidz fork 定制）
#
# 官方镜像的 /app/songloft 经 UPX 打包，在 DSM 6.2 / kernel 3.10（DS416play）上
# 必现 Segmentation fault（rc=139）。本镜像换用**未加壳**的二进制
# （Makefile 同参数构建、仅去 UPX），实测 NAS 上正常。
#
# 构建（WSL 内，仓库根目录）：
#   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOAMD64=v1 go build \
#     -tags lite -ldflags "-s -w -X songloft/internal/version.Version=$(ver)" \
#     -o bin/songloft-linux-amd64 .
#   docker build -f Dockerfile.nas -t songloft-fork:latest .
#
# 注意：-tags lite 不嵌 Web 前端（clients/player-build/web-embedded 需上游 CI 前端产物），
#       管理走手机 App。开源构建 ldflags 不注入 TRACELY_* → 遥测客户端不初始化（PRIVACY.md 行为）。
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata ffmpeg su-exec

ENV TZ=Asia/Shanghai
WORKDIR /app

# bin/songloft-linux-amd64      = lite（无 Web 前端，-tags lite）
# bin/songloft-linux-amd64-full = full（嵌入 Lynx Web：make build-frontend-web-embedded 后构建）
# 构建变体：docker build -f Dockerfile.nas --build-arg SONGLOFT_BIN=bin/songloft-linux-amd64-full -t songloft-fork:full .
ARG SONGLOFT_BIN=bin/songloft-linux-amd64
COPY ${SONGLOFT_BIN} /app/songloft
RUN chmod +x /app/songloft && mkdir -p /app/music /app/data

EXPOSE 58091

CMD ["/app/songloft"]
