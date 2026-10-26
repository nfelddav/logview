#!/usr/bin/env bash
# Cross-compiles static logview binaries for Linux into dist/.
# Needs only a Go toolchain; works on Linux, macOS and Git Bash on Windows.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
mkdir -p dist

# name GOARCH extra-env
targets=(
  "amd64   amd64   GOAMD64=v1"
  "arm64   arm64   "
  "armhf   arm     GOARM=7"
  "riscv64 riscv64 "
)

for t in "${targets[@]}"; do
  read -r name arch extra <<<"$t"
  out="dist/logview-linux-$name"
  echo "building $out"
  (
    export CGO_ENABLED=0 GOOS=linux GOARCH="$arch"
    if [ -n "$extra" ]; then export "${extra?}"; fi
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" .
  )
done

go run ./scripts/checkelf dist/logview-linux-*
( cd dist && sha256sum logview-linux-* > SHA256SUMS 2>/dev/null || shasum -a 256 logview-linux-* > SHA256SUMS )
ls -l dist
