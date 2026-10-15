#!/bin/sh
# Builds static binaries into dist/:
#   cubby-macos          universal (Apple Silicon and Intel)
#   cubby-linux-amd64    most servers and PCs
#   cubby-linux-arm64    Raspberry Pi 4/5 on a 64-bit OS, ARM servers
set -eu
cd "$(dirname "$0")"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
LDFLAGS="-s -w -X github.com/timjaanson/cubby/internal/version.Version=$VERSION"
export CGO_ENABLED=0

rm -rf dist && mkdir dist

build() {
  echo "building $1/$2"
  GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "$LDFLAGS" -o "dist/$3" ./cmd/cubby
}

build linux amd64 cubby-linux-amd64
build linux arm64 cubby-linux-arm64
build darwin arm64 cubby-darwin-arm64
build darwin amd64 cubby-darwin-amd64

if command -v lipo > /dev/null; then
  lipo -create -output dist/cubby-macos dist/cubby-darwin-arm64 dist/cubby-darwin-amd64
  rm dist/cubby-darwin-arm64 dist/cubby-darwin-amd64
fi

(cd dist && shasum -a 256 cubby-* > SHA256SUMS)
ls -lh dist
