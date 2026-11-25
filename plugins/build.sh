#!/usr/bin/env bash
# Builds the Go runner plugins in place, next to their plugin.json.
# Usage: plugins/build.sh [runner...]
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
runners=("$@")
if ((${#runners[@]} == 0)); then
  runners=(cucumber cypress gotest jest playwright pytest rspec vitest)
fi
for runner in "${runners[@]}"; do
  go build -o "plugins/$runner/go/bktest-runner-$runner" "./plugins/$runner/go"
done
