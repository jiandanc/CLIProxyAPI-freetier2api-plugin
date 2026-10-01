#!/usr/bin/env bash
# 本地构建：先跑测试，再产出平台对应的动态库。
#
# 本机没有 Go 工具链（只有 docker 镜像里有），因此统一经容器构建。
# 用 golang:1.24（Debian 版，自带 gcc）。
#
# **不要用 alpine 版**：宿主 CPA 镜像是 Debian(glibc)，alpine(musl) 构建出的
# .so 会以 "libc.musl-aarch64.so.1: cannot open shared object file" 加载失败。
# glibc 版的动态库依赖 libc.so.6 / ld-linux-*.so，与宿主一致。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="golang:1.24"

command -v docker >/dev/null 2>&1 || {
  echo "Docker 是必需的（本机没有 Go 工具链）。" >&2
  exit 1
}

# 动态库扩展名按目标平台决定；产物名必须与 pluginID 一致（freetier2api）。
ext="so"
case "${GOOS:-$(docker run --rm "$IMAGE" go env GOOS)}" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
  *) ext="so" ;;
esac

# -s -w 去掉符号表与 DWARF 调试信息：12.5MB → 9.1MB（约 -27%）。
# 代价是 panic 堆栈只剩函数名与地址，没有文件名/行号——定位线上问题时
# 需要先用这份源码在本地复现，或临时用不带该参数的构建还原堆栈。
# 宿主容器里没有 strip 命令，因此这一步必须在构建期完成，无法事后剥离。
LDFLAGS="-s -w"

mkdir -p dist
docker run --rm \
  -v "$ROOT":/w \
  -v freetier-gocache:/gocache \
  -w /w \
  -e GOCACHE=/gocache \
  -e CGO_ENABLED=1 \
  -e GOFLAGS=-buildvcs=false \
  "$IMAGE" sh -c "
    go test ./... -count=1 &&
    go build -buildmode=c-shared -ldflags='${LDFLAGS}' -o dist/freetier2api.${ext} .
  "

echo "Built dist/freetier2api.${ext}"