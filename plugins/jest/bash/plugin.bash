#!/usr/bin/env bash
# Bash runner plugin for Jest.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

plugin_initialize() {
  jq '.config
    | .test_command = (.test_command // "npx jest --runTestsByPath {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}")
    | .test_file_pattern = (.test_file_pattern // "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}")
    | .retry_test_command = (.retry_test_command // "npx jest --testNamePattern '"'"'{{testNamePattern}}'"'"' --json --testLocationInResults --outputFile {{resultPath}}")' <<<"$BK_PARAMS" | bk_save_config
  jq '{
    protocol_version: 1,
    name: "Jest",
    features: {split_by_file: true, filter_test_files: true, auto_retry: true, mute: true, split_by_selector: true},
    result_format: "jest-json",
    result_path: (.result_path // ""),
    location_prefix: (.location_prefix // "")
  }' "$BK_STATE_DIR/config.json"
}

plugin_discover() {
  bk_discover_files
}

plugin_examples() {
  bk_fail "jest does not support split by example"
}

# Retries run by an anchored name pattern of the failed tests' full names,
# which Jest joins from the describe titles and the title with spaces.
plugin_command() {
  local -a words paths
  bk_test_command words
  mapfile -t paths < <(bk_paths | LC_ALL=C sort -u)
  if [[ $(bk_param .retry) == true ]]; then
    local pattern
    # shellcheck disable=SC2016
    pattern=$(jq -r '"^(" + ([.test_cases[]
      | (if (.scope // "") != "" then .scope + " " + (.name // "") else (.name // "") end)
      | gsub("(?<c>[\\\\.+*?()|\\[\\]{}^$])"; "\\\(.c)")] | join("|")) + ")$"' <<<"$BK_PARAMS")
    bk_splice words '{{testNamePattern}}' "$pattern" || bk_fail "couldn't find '{{testNamePattern}}' sentinel in retry command"
    bk_splice words '{{testExamples}}' "${paths[@]}" || true
  else
    bk_splice words '{{testExamples}}' "${paths[@]}" || words+=("${paths[@]}")
  fi
  bk_splice words '{{resultPath}}' "$(bk_cfg result_path)" || bk_fail "couldn't find '{{resultPath}}' sentinel in command, exiting"
  bk_command_result "${words[@]}"
}

# A failed file without tests had a runtime error, such as one while
# loading; Jest still ran the other files, so their failures are retried.
# On retries, Jest reports the tests the name pattern didn't match as
# pending, so they keep their earlier result.
plugin_interpret() {
  local report
  report=$(bk_cfg result_path)
  if ! jq -e '.testResults | type == "array"' "$report" >/dev/null 2>&1; then
    jq -n --arg path "$report" '{messages: ["Buildkite Test Engine Client: Failed to read Jest output, tests will not be retried: failed to read report output from \($path)"]}'
    return
  fi
  # shellcheck disable=SC2016
  jq --arg cwd "$PWD" --argjson retry "$(bk_param .retry)" '
    def rel: if startswith($cwd + "/") then .[($cwd | length) + 1:] else . end;
    [.testResults[] | (.name | rel) as $path | {file: ., $path}] as $files
    | {
      errored_files: [$files[] | select(.file.status == "failed" and (.file.assertionResults | length) == 0) | .path],
      tests: [$files[] | .path as $path | .file.assertionResults[]
        | {status: ({failed: "failed", passed: "passed", pending: "skipped", todo: "skipped"}[.status] // "unknown"),
           test_case: {name: .title, scope: (.ancestorTitles | join(" ")), path: $path}}
        | select(($retry and .status == "skipped") | not)]
    }' "$report"
}

bk_serve
