#!/usr/bin/env bash
# 在容器里跑本项目的 Go 命令（本机没有 Go 工具链，只有 docker 镜像里有）。
#
# 用 golang:1.24（Debian 版，自带 gcc，cgo 构建 c-shared 需要 C 编译器）。
#
# **不要用 alpine 版**：宿主 CPA 镜像是 Debian(glibc)，alpine(musl) 构建的
# .so 会以 "libc.musl-aarch64.so.1: cannot open shared object file" 加载失败。
#
# 用法：
#   ./scripts/godocker.sh test ./...        # go test ./...
#   ./scripts/godocker.sh build ./...       # go build ./...
#   ./scripts/godocker.sh vet ./...         # go vet ./...
#   ./scripts/godocker.sh run ./scripts/packstore ...  # go run
set -euo pipefail

IMAGE="golang:1.24"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# GOCACHE/GOMODCACHE 落在宿主挂载卷外的匿名卷里，避免与宿主的 GOPATH 冲突，
# 同时让容器内的编译缓存跨次运行复用（否则每次都要重新编译全部依赖）。
exec docker run --rm \
  -v "$ROOT":/w \
  -v freetier-gocache:/gocache \
  -w /w \
  -e GOCACHE=/gocache \
  -e CGO_ENABLED=1 \
  -e GOFLAGS=-buildvcs=false \
  "$IMAGE" go "$@"
