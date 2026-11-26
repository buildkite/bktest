#!/usr/bin/env bash
# Builds the Go runner plugins: plugins/<runner>/go/bktest-runner-<runner>.
# With arguments, builds only those runners.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

runners=("$@")
if [[ ${#runners[@]} -eq 0 ]]; then
  for dir in */go; do
    runners+=("${dir%/go}")
  done
fi
for runner in "${runners[@]}"; do
  go build -o "$runner/go/bktest-runner-$runner" "./$runner/go"
done
