#!/usr/bin/env bash
# Bash runner plugin for Playwright.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

plugin_initialize() {
  jq '.config
    | .test_command = (.test_command // "npx playwright test")
    | .retry_test_command = (.retry_test_command // .test_command)
    | .test_file_pattern = (.test_file_pattern // "**/{*.spec,*.test}.{ts,js}")' <<<"$BK_PARAMS" | bk_save_config
  # Skip stays off: a file:line runs in every project, so skipping one
  # project's example would still run the others.
  jq '{
    protocol_version: 1,
    name: "Playwright",
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      auto_retry: true, mute: true, split_by_selector: true},
    result_format: (if (.result_path // "") != "" then "playwright-json" else "" end),
    result_path: (.result_path // ""),
    location_prefix: (.location_prefix // "")
  }' "$BK_STATE_DIR/config.json"
}

plugin_discover() {
  bk_discover_files
}

playwright_argv() {
  local -n _argv=$1
  local -a paths
  mapfile -t paths
  bk_test_command _argv
  bk_splice _argv '{{testExamples}}' "${paths[@]}" || _argv+=("${paths[@]}")
}

plugin_command() {
  local -a argv
  playwright_argv argv < <(bk_paths)
  bk_command_result "${argv[@]}"
}

# Specs with their suite titles joined by spaces, and the test case the
# Buildkite Playwright collector reports: its scope is the title path.
# shellcheck disable=SC2016
PLAYWRIGHT_DEFS='
  def specs($name): (.specs[]? | {spec: ., suite: $name}),
    (.suites[]? as $child | $child | specs($name + " " + $child.title));
  def all_specs: [.suites[]? | . as $s | specs($s.title)];
  def test_case: {name: .spec.title, path: "\(.spec.file):\(.spec.line)",
    scope: " \(.spec.tests[0].projectName) \(.suite) \(.spec.title)"};
'

# Examples lists tests with --list. The JSON reporter's output file can only
# be set with an environment variable.
plugin_examples() {
  local report output
  local -a argv files
  mapfile -t files < <(bk_param '.files[]')
  if ((${#files[@]} == 0)); then
    echo '{"test_cases": []}'
    return
  fi
  report=$(mktemp "${TMPDIR:-/tmp}/playwright-list-XXXXXX.json")
  playwright_argv argv < <(printf '%s\n' "${files[@]}")
  argv+=(--list --reporter=json)
  bk_log "Running \`${argv[*]}\` to list tests"
  if ! output=$(PLAYWRIGHT_JSON_OUTPUT_NAME="$report" "${argv[@]}" 2>&1 >/dev/null </dev/null); then
    rm -f "$report"
    bk_fail "failed to list playwright tests: $output"
  fi
  jq "$PLAYWRIGHT_DEFS {test_cases: [all_specs[] | test_case]}" "$report" || bk_fail "failed to parse playwright test list output"
  rm -f "$report"
}

# A test skipped because something else failed didn't run, and a passing
# retry must not hide that. When test files fail to load, Playwright runs
# nothing and locates the errors in those files: they error, and the other
# files run again. Any other error errors the whole run.
plugin_interpret() {
  local report
  report=$(bk_cfg result_path)
  if ! jq -e 'type == "object"' "$report" >/dev/null 2>&1; then
    jq -n --arg path "$report" '{messages: ["Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: failed to read playwright output from \($path)"]}'
    return
  fi
  printf '%s' "$BK_PARAMS" >"$BK_STATE_DIR/params.json"
  # shellcheck disable=SC2016
  jq --arg cwd "$PWD" --slurpfile params "$BK_STATE_DIR/params.json" "$PLAYWRIGHT_DEFS"'
    def rel: if startswith($cwd + "/") then .[($cwd | length) + 1:] else . end;
    {tests: [all_specs[] | {test_case: test_case,
      status: (.spec.ok as $ok | .spec.tests as $tests
        | if ($ok | not) then "failed" elif $tests[0].status == "skipped" then "skipped" else "passed" end
        | if $ok and any($tests[]; .status == "skipped" and .expectedStatus != "skipped") then "not_run" else . end)}
      ]} as $result
    | ((.errors // []) | length) as $errors
    | if $errors == 0 then $result
      else
        (all(.errors[]; (.location.file // "") != "")) as $located
        | ([.errors[].location.file // "" | rel] | reduce .[] as $f ([]; if index([$f]) then . else . + [$f] end)) as $files
        | [$params[0].test_cases[] | {tc: ., file: ((if .format == "selector" then .value else .path end) | sub(":[0-9]+$"; "") | ltrimstr("./"))}] as $cases
        | ([$files[] | select(. as $f | any($cases[]; .file == $f) | not)] | length) as $unmatched
        | if ($located | not) or ((.suites // []) | length) > 0 or $unmatched > 0 then $result + {error: "Playwright failed with errors"}
          else
            [$cases[] | select(.file as $f | $files | index([$f]) | not) | .tc] as $remaining
            | $result + {errored_files: $files}
            + if ($remaining | length) > 0 then
                {rerun: $remaining, messages: ["+++ Buildkite Test Engine Client: Playwright failed to load \($files | join(", ")). Running the other test files again."]}
              else {} end
          end
      end' "$report"
}

bk_serve
