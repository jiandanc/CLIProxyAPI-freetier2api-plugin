#!/usr/bin/env bash
# 在容器里跑本项目的 Go 命令（本机没有 Go 工具链，只有 docker 镜像里有）。
#
# 用 freetier-build:1.24（golang:1.24-alpine + gcc + musl-dev），
# 因为 cgo 构建 c-shared 需要 C 编译器，而官方 alpine 镜像不带。
#
# 用法：
#   ./scripts/godocker.sh test ./...        # go test ./...
#   ./scripts/godocker.sh build ./...       # go build ./...
#   ./scripts/godocker.sh vet ./...         # go vet ./...
#   ./scripts/godocker.sh run ./scripts/packstore ...  # go run
set -euo pipefail

IMAGE="freetier-build:1.24"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "构建镜像 $IMAGE（只需一次）…" >&2
  docker build -t "$IMAGE" - <<'EOF' >&2
FROM golang:1.24-alpine
RUN apk add --no-cache gcc musl-dev git
EOF
fi

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
