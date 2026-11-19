#!/usr/bin/env bash
set -euo pipefail

version=$1
out=$2
os=$3
arch=$4
ext=
if [[ "$os" == windows ]]; then
  ext=.exe
fi

GOWORK=off CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags "-X github.com/buildkite/bktest/v3/internal/version.Version=v$version" \
  -o "$out/bktest_${version}_${os}_${arch}${ext}" ./cmd/bktest
