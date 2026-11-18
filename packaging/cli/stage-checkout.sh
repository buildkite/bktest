#!/usr/bin/env bash
set -euo pipefail

out=$1
tag=${BUILDKITE_TAG:-}
source=$(git rev-parse HEAD)
if [[ -n "$tag" ]]; then
  version=${tag#v}
  PYTHONPATH=packaging/cli python3 -B -c 'import re, sys; from stage import VERSION; sys.exit(not re.fullmatch("v" + VERSION, sys.argv[1]))' "$tag"
  [[ $(git rev-parse --verify "refs/tags/${tag}^{commit}") == "$source" ]] || {
    echo "Release tag does not point at the checked-out commit" >&2
    exit 1
  }
  [[ -z $(git status --porcelain) ]] || {
    echo "Release tag staging requires a clean checkout" >&2
    exit 1
  }
else
  version=3.0.0-rehearsal
fi

export SOURCE_DATE_EPOCH
SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)
payloads=$(mktemp -d)
trap 'rm -rf "$payloads"' EXIT
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    bash packaging/cli/build.sh "$version" "$payloads" "$os" "$arch"
  done
done
python3 -B packaging/cli/stage.py "$version" "$source" "$payloads" "$out"
