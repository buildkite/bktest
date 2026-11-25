#!/usr/bin/env bash
# Bash runner plugin for Cucumber (Ruby).
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

plugin_initialize() {
  jq '.config
    | .test_command = (.test_command // "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}")
    | .test_file_pattern = (.test_file_pattern // "features/**/*.feature")
    | .retry_test_command = (.retry_test_command // .test_command)' <<<"$BK_PARAMS" | bk_save_config
  jq '{
    protocol_version: 1,
    name: "Cucumber",
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      auto_retry: true, mute: true, skip: true, split_by_selector: true},
    result_format: "cucumber-json",
    result_path: (.result_path // ""),
    location_prefix: (.location_prefix // "")
  }' "$BK_STATE_DIR/config.json"
}

plugin_discover() {
  bk_discover_files
}

cucumber_argv() {
  local -n _argv=$1
  local -a paths
  mapfile -t paths
  bk_test_command _argv
  bk_splice _argv '{{testExamples}}' "${paths[@]}" || _argv+=("${paths[@]}")
  bk_splice _argv '{{resultPath}}' "$(bk_cfg result_path)" || true
}

plugin_command() {
  local -a argv
  cucumber_argv argv < <(bk_paths)
  bk_command_result "${argv[@]}"
}

# Scenarios are identified by feature file and line.
# shellcheck disable=SC2016
SCENARIO_TEST_CASE='((.feature.uri + ":" + (.scenario.line | tostring)) as $location
  | {identifier: $location, name: .scenario.name, scope: .feature.name, path: $location})'

# Examples dry-runs Cucumber with the test command's executable.
plugin_examples() {
  local report output
  local -a argv files
  mapfile -t files < <(bk_param '.files[]')
  if ((${#files[@]} == 0)); then
    echo '{"test_cases": []}'
    return
  fi
  report=$(mktemp "${TMPDIR:-/tmp}/cucumber-dry-run-XXXXXX.json")
  cucumber_argv argv < <(printf '%s\n' "${files[@]}")
  bk_log "Running \`${argv[0]} --dry-run --format json --out $report --format progress ${files[*]}\` for dry run"
  if ! output=$("${argv[0]}" --dry-run --format json --out "$report" --format progress "${files[@]}" 2>&1 </dev/null); then
    rm -f "$report"
    bk_fail "failed to run Cucumber dry run: $output"
  fi
  jq -s "(.[0] // []) | (if type == \"array\" then . else [.] end)
    | {test_cases: [.[] as \$feature | \$feature.elements[]? | select(.type == \"scenario\")
      | {feature: \$feature, scenario: .} | $SCENARIO_TEST_CASE]}" "$report" || bk_fail "failed to parse cucumber dry run JSON report"
  rm -f "$report"
}

# A scenario failed if any step failed or is undefined, was skipped if any
# step is pending or skipped, and otherwise passed.
plugin_interpret() {
  local report
  report=$(bk_cfg result_path)
  if ! jq -e 'type == "array"' "$report" >/dev/null 2>&1; then
    jq -n --arg path "$report" '{messages: ["Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: failed to read cucumber output from \($path)"]}'
    return
  fi
  jq "{tests: [.[] as \$feature | \$feature.elements[]? | select(.type == \"scenario\")
    | {feature: \$feature, scenario: .}
    | {test_case: $SCENARIO_TEST_CASE,
       status: ([.scenario.steps[]?.result.status? // empty] as \$s
         | if any(\$s[]; . == \"failed\" or . == \"undefined\" or . == \"errored\") then \"failed\"
           elif any(\$s[]; . == \"pending\" or . == \"skipped\") then \"skipped\"
           else \"passed\" end)}]}" "$report"
}

bk_serve
