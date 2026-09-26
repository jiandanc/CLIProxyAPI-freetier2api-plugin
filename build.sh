#!/usr/bin/env bash
# 本地构建：先跑测试，再产出平台对应的动态库。
set -euo pipefail

command -v go >/dev/null 2>&1 || {
  echo "Go 1.22 or newer is required." >&2
  exit 1
}

# 动态库扩展名按平台决定；产物名必须与 pluginID 一致（workbuddy2api）。
case "$(go env GOOS)" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
  *) ext="so" ;;
esac

mkdir -p dist
go test ./... -count=1
CGO_ENABLED=1 go build \
  -buildvcs=false \
  -buildmode=c-shared \
  -o "dist/workbuddy2api.${ext}" \
  .

echo "Built dist/workbuddy2api.${ext}"
