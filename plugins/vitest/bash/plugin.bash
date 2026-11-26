#!/usr/bin/env bash
# Bash runner plugin for Vitest. Its JSON reporter writes the same report as
# Jest's.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
VITEST_DEFS='
# What identifies a test across attempts: its scope, name and path.
def vitest_key: "\(.scope // "")/\(.name // "")/\(.path // "")";

# The pattern that matches the full names of the test cases, for a retry. It
# is anchored so only the failed tests run, not every test whose name contains
# one of theirs. $names has the full names of the tests in earlier reports, by
# key. A test case that was in no report only has its scope, which joins the
# suite titles with spaces, so the suites cannot be told apart.
def vitest_name_pattern($names):
  [.test_cases[]
   | ($names[vitest_key] // [(if (.scope // "") != "" then .scope + " " else "" end) + (.name // "")])[]
   | bk_regex_quote]
  | "^(" + bk_join("|") + ")$";

# The test command for the test cases of the request. A retry runs the failed
# tests by name, in all files unless the retry command names the files.
def vitest_argv($names):
  . as $request
  | (bk_paths | map(. // "") | unique) as $paths
  | bk_test_command | bk_words
  | if $request.retry then
      if bk_index("{{testNamePattern}}") == null
      then error("couldn\u0027t find \u0027{{testNamePattern}}\u0027 sentinel in retry command") else . end
      | bk_splice("{{testNamePattern}}"; [$request | vitest_name_pattern($names)])
      | bk_splice("{{testExamples}}"; $paths)
    elif bk_index("{{testExamples}}") == null then . + $paths
    else bk_splice("{{testExamples}}"; $paths) end
  | if bk_index("{{resultPath}}") == null
    then error("couldn\u0027t find \u0027{{resultPath}}\u0027 sentinel in command, exiting") else . end
  | bk_splice("{{resultPath}}"; [$request.config.result_path // ""]);

# What the report says about an attempt: the interpret-attempt response, then
# $names with the full names of the tests of the report.
#
# A failed file without assertions failed outside its tests, such as an
# import that throws, which no retry fixes. A retry runs tests by name, and
# a test the pattern does not match is reported as skipped: it keeps its
# earlier result. Vitest can exit 0 after a retry while tests of an earlier
# attempt are still failed, so bktest is asked to fail the attempt then.
#
# The scope and name have to match the Buildkite test collector for Vitest,
# so the full name is kept apart. Vitest 4 matches names that join the titles
# with spaces and Vitest 5 with " > ", so a retry gives both.
def vitest_attempt($cwd; $retry; $names):
  [.testResults | bk_array | .[] | {
    name: (.name | bk_string),
    status: .status,
    assertions: (.assertionResults | bk_array)
  }] as $files
  | [$files[] | select((.assertions | length) > 0) | (.name | bk_rel($cwd)) as $path | .assertions[] | {
      test_case: {name: (.title | bk_string), scope: (.ancestorTitles | bk_array | join(" ")), path: $path},
      status: ({failed: "failed", passed: "passed", pending: "skipped", skipped: "skipped",
        todo: "skipped"}[.status | bk_string] // "unknown"),
      titles: ((.ancestorTitles | bk_array) + [.title | bk_string])
    } | select(($retry and .status == "skipped") | not)] as $tests
  | {
      tests: [$tests[] | {test_case: .test_case, status: .status}],
      failed_tests_message: "Vitest finished with failed tests; see the report above"
    }
    + (if any($files[]; .status == "failed" and (.assertions | length) == 0)
       then {run_error: "Vitest failed with runtime error test suites"} else {} end),
    $names + ([$tests[] | {
      (.test_case | vitest_key): (.titles | if length > 1 then [join(" "), join(" > ")] else [join(" ")] end)
    }] | add // {});
'

# Prints the file with the full names of the tests in the reports of earlier
# attempts, which retries need: a JSON object from the key of a test to its
# names. Before the first report, it is an empty file.
vitest_names() {
  local names="$BK_STATE_DIR/vitest-retry-names.json"
  [[ -e $names ]] || names=/dev/null
  printf '%s' "$names"
}

plugin_configure() {
  bk_request '{
    name: "Vitest",
    config: (.config
      | bk_default("test_command"; "npx vitest run {{testExamples}} --reporter=default --reporter=json --outputFile {{resultPath}}")
      | bk_default("test_file_pattern"; "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}")
      | bk_default("retry_test_command"; "npx vitest run --testNamePattern \u0027{{testNamePattern}}\u0027 --reporter=default --reporter=json --outputFile {{resultPath}}")),
    features: {split_by_file: true, filter_test_files: true, auto_retry: true, mute: true, split_by_selector: true},
    result_format: "jest-json"
  }'
}

plugin_prepare_attempt() {
  # shellcheck disable=SC2016
  bk_request "$VITEST_DEFS"'{argv: vitest_argv($names[0] // {})}' --slurpfile names "$(vitest_names)"
}

# Vitest exits with a failure when tests fail, so the report is read whatever
# the command's exit status was.
plugin_interpret_attempt() {
  local report retry reason status=0
  report=$(bk_cfg result_path)
  retry=$(bk_request .retry)
  # shellcheck disable=SC2016
  bk_report "$report" "$VITEST_DEFS"'vitest_attempt($cwd; $retry; $names[0] // {})' --compact-output \
    --arg cwd "$PWD" --argjson retry "$retry" --slurpfile names "$(vitest_names)" >"$BK_TMP/attempt" || status=$?
  case $status in
    0)
      # The response is the first line and the names are the second.
      head -n 1 "$BK_TMP/attempt"
      tail -n 1 "$BK_TMP/attempt" >"$BK_TMP/names"
      mv "$BK_TMP/names" "$BK_STATE_DIR/vitest-retry-names.json"
      return
      ;;
    1) reason="failed to read report output: $BK_REPORT_ERROR" ;;
    *)
      # A file that the report doesn't name by absolute path fails the
      # attempt instead.
      if [[ $BK_REPORT_ERROR == "Rel: "* ]]; then
        bk_fail "failed to get relative path of test file: $BK_REPORT_ERROR"
      fi
      reason="failed to parse report output: $BK_REPORT_ERROR"
      ;;
  esac
  # Without a report nothing is retried, and the command's own exit status
  # decides the attempt, unless tests of an earlier attempt are still failed.
  jq -n --arg reason "$reason" '{
    messages: ["Buildkite Test Engine Client: Failed to read Vitest output, tests will not be retried: \($reason)"],
    failed_tests_message: "Vitest finished with failed tests; see the report above"
  }'
}

bk_main "$@"
