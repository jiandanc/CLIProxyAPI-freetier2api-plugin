#!/usr/bin/env bash
# 本地构建：先跑测试，再产出平台对应的动态库。
#
# 本机没有 Go 工具链（只有 docker 镜像里有），因此统一经容器构建。
# 需要的镜像 freetier-build:1.24 由 scripts/godocker.sh 自动构建
# （golang:1.24-alpine + gcc + musl-dev，cgo 构建 c-shared 需要 C 编译器）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="freetier-build:1.24"

command -v docker >/dev/null 2>&1 || {
  echo "Docker 是必需的（本机没有 Go 工具链）。" >&2
  exit 1
}

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "构建镜像 $IMAGE（只需一次）…" >&2
  docker build -t "$IMAGE" - <<'EOF' >&2
FROM golang:1.24-alpine
RUN apk add --no-cache gcc musl-dev git
EOF
fi

# 动态库扩展名按目标平台决定；产物名必须与 pluginID 一致（freetier2api）。
ext="so"
case "${GOOS:-$(docker run --rm "$IMAGE" go env GOOS)}" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
  *) ext="so" ;;
esac

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
    go build -buildmode=c-shared -o dist/freetier2api.${ext} .
  "

echo "Built dist/freetier2api.${ext}"