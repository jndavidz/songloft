#!/usr/bin/env bash
# 发布证据清单：把「镜像 ↔ HEAD ↔ 依赖 ↔ 运行时组件」固化成可复查的一行行事实。
#
# 用法（WSL，仓库根）：
#   scripts/release-manifest.sh songloft-fork:<tag> [输出文件]
#
# 需要 docker（WSL 侧）。任何一步取不到值都写 "unavailable"，不伪造。
set -euo pipefail

IMAGE=${1:?usage: scripts/release-manifest.sh <image-ref> [output.txt]}
OUT=${2:-release-manifest.txt}
ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

git_commit=$(git rev-parse HEAD)
git_describe=$(git describe --tags --always 2>/dev/null || echo "$git_commit")
git_dirty=$(git status --porcelain | wc -l | tr -d ' ')
go_sum=$(sha256sum go.sum | awk '{print $1}')
player_commit=$(git -C clients/player rev-parse HEAD 2>/dev/null || echo unavailable)
image_id=$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || echo unavailable)
repo_digests=$(docker image inspect --format '{{join .RepoDigests ","}}' "$IMAGE" 2>/dev/null || echo unavailable)
base_ref=$(awk '/^FROM /{print $2; exit}' Dockerfile.nas)
base_digest=$(docker manifest inspect "$base_ref" 2>/dev/null | sha256sum | awk '{print $1}' || echo unavailable)
pkg_fingerprint=$(docker run --rm "$IMAGE" sh -c 'apk info -v | sort' 2>/dev/null | sha256sum | awk '{print $1}' || echo unavailable)
ffmpeg_version=$(docker run --rm "$IMAGE" sh -c 'ffmpeg -version 2>/dev/null | head -1' 2>/dev/null || echo unavailable)
go_version=$(go version 2>/dev/null || echo unavailable)
apk_path=${SONGLOFT_APK_PATH:-}
apk_sha1=unavailable
if [ -n "$apk_path" ] && [ -f "$apk_path" ]; then
  apk_sha1=$(sha1sum "$apk_path" | awk '{print $1}')
fi

{
  echo "generated_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "image=$IMAGE"
  echo "image_id=$image_id"
  echo "image_repo_digests=$repo_digests"
  echo "base_image=$base_ref"
  echo "base_manifest_sha256=$base_digest"
  echo "image_packages_sha256=$pkg_fingerprint"
  echo "ffmpeg=$ffmpeg_version"
  echo "git_commit=$git_commit"
  echo "git_describe=$git_describe"
  echo "git_dirty_files=$git_dirty"
  echo "go_sum_sha256=$go_sum"
  echo "go_toolchain=$go_version"
  echo "player_submodule=$player_commit"
  echo "apk_sha1=$apk_sha1"
} | tee "$OUT"
