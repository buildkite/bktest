#!/usr/bin/env bash
set -euo pipefail

export CI=true BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false
unset BUILDKITE_ANALYTICS_TOKEN BUILDKITE_TEST_ENGINE_API_ACCESS_TOKEN

case "$1" in
  go|python)
    python3 -m pip install ./test-collector-python
    if [[ "$1" == python ]]; then
      go test -race -count=1 -v -run '^(TestPytest|TestPoolConsumePytest)' ./internal/runner ./internal/command
    else
      (cd internal/runner/testdata && yarn install --frozen-lockfile)
      (cd internal/runner/testdata/playwright && yarn playwright install --with-deps)
      mkdir -p dist/coverage
      go test -race -count=1 -coverprofile=dist/coverage/cover.out ./...
      go tool cover -html dist/coverage/cover.out -o dist/coverage/cover.html
    fi
    ;;
  ruby)
    export BKTEC_RSPEC_RUNNER_ROOT="$PWD/test-collector-ruby"
    go test -race -count=1 -tags=integration ./internal/integration/...
    ;;
  *) exit 2 ;;
esac
