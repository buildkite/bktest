#!/usr/bin/env bash
# Bash runner plugin for Vitest.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# Exact names learned from earlier reports, by test identity. Retries need
# them because suite boundaries can't be recovered from the scope.
RETRY_NAMES="$BK_STATE_DIR/retry-names.json"
echo '{}' >"$RETRY_NAMES"

plugin_initialize() {
  jq '.config
    | .test_command = (.test_command // "npx vitest run {{testExamples}} --reporter=default --reporter=json --outputFile {{resultPath}}")
    | .test_file_pattern = (.test_file_pattern // "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}")
    | .retry_test_command = (.retry_test_command // "npx vitest run --testNamePattern '"'"'{{testNamePattern}}'"'"' --reporter=default --reporter=json --outputFile {{resultPath}}")' <<<"$BK_PARAMS" | bk_save_config
  jq '{
    protocol_version: 1,
    name: "Vitest",
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
  bk_fail "vitest does not support split by example"
}

plugin_command() {
  local -a words paths
  bk_test_command words
  mapfile -t paths < <(bk_paths | LC_ALL=C sort -u)
  if [[ $(bk_param .retry) == true ]]; then
    local pattern
    # Anchor the names so only the failed tests run.
    # shellcheck disable=SC2016
    pattern=$(jq -r --slurpfile names "$RETRY_NAMES" '"^(" + ([.test_cases[]
      | ($names[0][(.scope // "") + "/" + (.name // "") + "/" + .path]
         // [if (.scope // "") != "" then .scope + " " + (.name // "") else (.name // "") end])[]
      | gsub("(?<c>[\\\\.+*?()|\\[\\]{}^$])"; "\\\(.c)")] | join("|")) + ")$"' <<<"$BK_PARAMS")
    bk_splice words '{{testNamePattern}}' "$pattern" || bk_fail "couldn't find '{{testNamePattern}}' sentinel in retry command"
    bk_splice words '{{testExamples}}' "${paths[@]}" || true
  else
    bk_splice words '{{testExamples}}' "${paths[@]}" || words+=("${paths[@]}")
  fi
  bk_splice words '{{resultPath}}' "$(bk_cfg result_path)" || bk_fail "couldn't find '{{resultPath}}' sentinel in command, exiting"
  bk_command_result "${words[@]}"
}

# Vitest can exit 0 with failed tests, so the result asks bktest to fail the
# attempt then. A failed file without tests had a runtime error, such as an
# import that throws, which retrying can't fix.
plugin_interpret() {
  local report result
  report=$(bk_cfg result_path)
  if ! jq -e '.testResults | type == "array"' "$report" >/dev/null 2>&1; then
    jq -n --arg path "$report" '{
      failed_tests_message: "Vitest finished with failed tests; see the report above",
      messages: ["Buildkite Test Engine Client: Failed to read Vitest output, tests will not be retried: failed to read report output from \($path)"]
    }'
    return
  fi
  # shellcheck disable=SC2016
  result=$(jq -c --arg cwd "$PWD" --argjson retry "$(bk_param .retry)" '
    def rel: if startswith($cwd + "/") then .[($cwd | length) + 1:] else . end;
    {
      failed_tests_message: "Vitest finished with failed tests; see the report above",
      tests: [.testResults[] | (.name | rel) as $path | .assertionResults[]
        | {status: ({failed: "failed", passed: "passed", pending: "skipped", skipped: "skipped", todo: "skipped"}[.status] // "unknown"),
           test_case: {name: .title, scope: (.ancestorTitles | join(" ")), path: $path},
           titles: (.ancestorTitles + [.title])}
        | select(($retry and .status == "skipped") | not)]
    }
    + (if any(.testResults[]; .status == "failed" and (.assertionResults | length) == 0)
       then {error: "Vitest failed with runtime error test suites"} else {} end)' "$report") || bk_fail "failed to parse report output"
  # Vitest 4 matches space-separated names; Vitest 5 uses " > ".
  printf '%s' "$result" >"$BK_STATE_DIR/result.json"
  jq -c --slurpfile result "$BK_STATE_DIR/result.json" '. + ([$result[0].tests[]
    | {key: (.test_case.scope + "/" + .test_case.name + "/" + .test_case.path),
       value: ((.titles | join(" ")) as $spaces | (.titles | join(" > ")) as $arrows
         | if $spaces == $arrows then [$spaces] else [$spaces, $arrows] end)}] | from_entries)' "$RETRY_NAMES" >"$RETRY_NAMES.new" &&
    mv "$RETRY_NAMES.new" "$RETRY_NAMES"
  jq '.tests |= map(del(.titles))' "$BK_STATE_DIR/result.json"
}

bk_serve
