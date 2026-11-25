#!/usr/bin/env bash
# Bash runner plugin for RSpec.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

plugin_initialize() {
  jq '.config
    | .test_command = (.test_command // "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}")
    | .test_file_pattern = (.test_file_pattern // "spec/**/*_spec.rb")
    | .retry_test_command = (.retry_test_command // .test_command)
    | .location_prefix = (.location_prefix // "./")' <<<"$BK_PARAMS" | bk_save_config
  # Test Engine stores RSpec tests with a leading "./", hence the prefix.
  jq '{
    protocol_version: 1,
    name: "RSpec",
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      auto_retry: true, mute: true, skip: true, split_by_selector: true},
    result_format: "rspec-json",
    result_path: (.result_path // ""),
    location_prefix: .location_prefix
  }' "$BK_STATE_DIR/config.json"
}

plugin_discover() {
  bk_discover_files
}

# rspec_argv builds the command for the paths on stdin into the array $1.
rspec_argv() {
  local -n _argv=$1
  local -a paths
  mapfile -t paths
  bk_test_command _argv
  bk_splice _argv '{{testExamples}}' "${paths[@]}" || _argv+=("${paths[@]}")
  bk_splice _argv '{{resultPath}}' "$(bk_cfg result_path)" || true
}

plugin_command() {
  local -a argv
  rspec_argv argv < <(bk_paths)
  bk_command_result "${argv[@]}"
}

# The collector's scope is the example group's description, which the JSON
# report only has as the full description ending in the example's own.
# shellcheck disable=SC2016
RSPEC_TEST_CASE='{
  identifier: .id,
  name: .description,
  path: .id,
  scope: ((" " + .description) as $suffix
    | if (.full_description | endswith($suffix)) then .full_description[:-($suffix | length)] else .full_description end)
}'

# Examples dry-runs RSpec to list the examples with their runnable IDs.
plugin_examples() {
  local report output
  local -a argv
  report=$(mktemp "${TMPDIR:-/tmp}/dry-run-XXXXXX.json")
  rspec_argv argv < <(bk_param '.files[]')
  # Use the test command, not the retry command.
  argv+=(--dry-run --format json --out "$report" --format progress)
  bk_log "Running \`${argv[*]}\` for dry run"
  if ! output=$("${argv[@]}" 2>&1 </dev/null); then
    rm -f "$report"
    bk_fail "failed to run rspec dry run: $output"
  fi
  jq "{test_cases: [.examples[] | $RSPEC_TEST_CASE]}" "$report" || bk_fail "failed to parse rspec output"
  rm -f "$report"
}

# Each example is recorded under its ID without a leading "./", so retries
# target the exact example even when descriptions repeat.
plugin_interpret() {
  local report
  report=$(bk_cfg result_path)
  if ! jq -e '.examples | type == "array"' "$report" >/dev/null 2>&1; then
    jq -n --arg path "$report" '{messages: ["Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried: failed to read rspec output from \($path)"]}'
    return
  fi
  jq "{
    tests: [.examples[] | {
      test_case: $RSPEC_TEST_CASE,
      status: ({passed: \"passed\", failed: \"failed\", pending: \"skipped\"}[.status] // \"unknown\"),
      key: (.id | ltrimstr(\"./\"))
    }]
  } + (
    (.summary.errors_outside_of_examples_count // 0) as \$n
    | if \$n > 0 then {error: \"RSpec reported \(\$n) errors outside of examples\"} else {} end
  )" "$report"
}

bk_serve
