#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
for os in darwin linux; do
  for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "dist/aimount-$os-$arch" ./cmd/aimount
  done
done
if command -v sha256sum >/dev/null 2>&1; then
  (cd dist && sha256sum aimount-darwin-arm64 aimount-darwin-amd64 aimount-linux-arm64 aimount-linux-amd64 > SHA256SUMS)
else
  (cd dist && shasum -a 256 aimount-darwin-arm64 aimount-darwin-amd64 aimount-linux-arm64 aimount-linux-amd64 > SHA256SUMS)
fi
