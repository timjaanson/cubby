#!/bin/sh
# Builds static binaries into dist/:
#   tiny-kv-macos          universal (Apple Silicon and Intel)
#   tiny-kv-linux-amd64    most servers and PCs
#   tiny-kv-linux-arm64    Raspberry Pi 4/5 on a 64-bit OS, ARM servers
set -eu
cd "$(dirname "$0")"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
LDFLAGS="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0

rm -rf dist && mkdir dist

build() {
  echo "building $1/$2"
  GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "$LDFLAGS" -o "dist/$3" .
}

build linux amd64 tiny-kv-linux-amd64
build linux arm64 tiny-kv-linux-arm64
build darwin arm64 tiny-kv-darwin-arm64
build darwin amd64 tiny-kv-darwin-amd64

if command -v lipo > /dev/null; then
  lipo -create -output dist/tiny-kv-macos dist/tiny-kv-darwin-arm64 dist/tiny-kv-darwin-amd64
  rm dist/tiny-kv-darwin-arm64 dist/tiny-kv-darwin-amd64
fi

(cd dist && shasum -a 256 tiny-kv-* > SHA256SUMS)
ls -lh dist
