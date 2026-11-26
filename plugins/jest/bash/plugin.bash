#!/usr/bin/env bash
# Bash runner plugin for Jest.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
JEST_DEFS='
# The pattern that matches the full names of the test cases, for a retry.
# Jest matches a pattern anywhere in the full name of a test, which joins its
# describe titles and its title with spaces. The pattern is anchored so only
# the failed tests run, not every test whose name contains one of theirs.
def jest_name_pattern:
  [.test_cases[]
   | (if (.scope // "") != "" then .scope + " " else "" end) + (.name // "")
   | bk_regex_quote]
  | "^(" + bk_join("|") + ")$";

# The test command for the test cases of the request. A retry runs the failed
# tests by name, in all files unless the retry command names the files.
def jest_argv:
  . as $request
  | (bk_paths | map(. // "") | unique) as $paths
  | bk_test_command | bk_words
  | if $request.retry then
      if bk_index("{{testNamePattern}}") == null
      then error("couldn\u0027t find \u0027{{testNamePattern}}\u0027 sentinel in retry command") else . end
      | bk_splice("{{testNamePattern}}"; [$request | jest_name_pattern])
      | bk_splice("{{testExamples}}"; $paths)
    elif bk_index("{{testExamples}}") == null then . + $paths
    else bk_splice("{{testExamples}}"; $paths) end
  | if bk_index("{{resultPath}}") == null
    then error("couldn\u0027t find \u0027{{resultPath}}\u0027 sentinel in command, exiting") else . end
  | bk_splice("{{resultPath}}"; [$request.config.result_path // ""]);

# An assertion of the report as a test case of the file at $path. The scope
# and name have to match the Buildkite test collector for Jest.
def jest_test_case($path):
  {name: (.title | bk_string), scope: (.ancestorTitles | bk_array | join(" ")), path: $path};

# What the report says about an attempt, for the interpret-attempt response.
# A failed file without assertions failed outside its tests, such as while
# loading. Jest still runs the other files, so their failed tests can be
# retried. A retry runs tests by name, and Jest reports the tests that the
# pattern does not match as pending: those keep their earlier result.
def jest_attempt($cwd; $retry):
  [.testResults | bk_array | .[] | {
    path: (.name | bk_string | bk_rel($cwd)),
    status: .status,
    assertions: (.assertionResults | bk_array)
  }] as $files
  | {
      tests: [$files[] | .path as $path | .assertions[] | {
          test_case: jest_test_case($path),
          status: ({failed: "failed", passed: "passed", pending: "skipped", todo: "skipped"}[.status | bk_string] // "unknown")
        } | select(($retry and .status == "skipped") | not)],
      errored_files: [$files[] | select(.status == "failed" and (.assertions | length) == 0) | .path]
    };

# The input tests with those of the same file, scope and name merged into the
# first of them. Tests with the same name in one file, such as from
# test.each, cannot be told apart, so they are reported once: failed if any
# failed, and skipped only if all were skipped.
def jest_merge_repeated:
  [range(0; length) as $index | {index: $index, test: .[$index]}]
  | group_by(.test | [.selector, .test_case.scope, .test_case.name])
  | map(.[0] + {
      status: (reduce .[1:][].test.status as $status (.[0].test.status;
        if $status == "failed" or . == "skipped" then $status else . end))
    })
  | sort_by(.index)
  | map(.test + {status: .status});

# The report of a batch, for the parse-batch-report response.
def jest_batch($cwd):
  if .testResults == null then error("incomplete Jest report: missing testResults") else . end
  | (.numRuntimeErrorTestSuites // 0) as $runtime_errors
  | [.testResults | bk_array | .[]
     | (.name | bk_string | if startswith("/") then bk_rel($cwd) else . end | ltrimstr("./")) as $path
     | if $path == "" then error("jest test file has no name") else . end
     | {
         path: $path,
         status: .status,
         tests: [.assertionResults | bk_array | .[] | (.status | bk_string) as $status | {
           test_case: jest_test_case($path),
           status: ({passed: "passed", failed: "failed", pending: "skipped", skipped: "skipped",
             todo: "skipped", disabled: "skipped"}[$status] // error("unknown Jest test status \"\($status)\"")),
           selector: $path,
           location: (if (.location.line // 0) > 0 then "\($path):\(.location.line)" else "" end)
         }]
       }] as $files
  # A file that fails without a failed test, for example because it did not
  # load or an afterAll hook threw, failed outside its tests.
  | [$files[] | select(.status == "failed" and all(.tests[]; .status != "failed")) | .path] as $errored_files
  | {
      tests: ([$files[].tests[]] | jest_merge_repeated),
      errored_files: $errored_files,
      # A file can also fail outside its tests when some of its tests failed.
      # The report counts these files without naming them, so they error
      # every test of the batch.
      errors_outside_tests: ($runtime_errors > ($errored_files | length))
    };
'

plugin_configure() {
  bk_request '{
    name: "Jest",
    config: (.config
      | bk_default("test_command"; "npx jest --runTestsByPath {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}")
      | bk_default("test_file_pattern"; "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}")
      | bk_default("retry_test_command"; "npx jest --testNamePattern \u0027{{testNamePattern}}\u0027 --json --testLocationInResults --outputFile {{resultPath}}")),
    features: {split_by_file: true, filter_test_files: true, auto_retry: true, mute: true, split_by_selector: true},
    result_format: "jest-json"
  }'
}

plugin_prepare_attempt() {
  bk_request "$JEST_DEFS"'{argv: jest_argv}'
}

# Jest exits with a failure when tests fail, so the report is read whatever
# the command's exit status was.
plugin_interpret_attempt() {
  local report retry reason status=0
  report=$(bk_cfg result_path)
  retry=$(bk_request .retry)
  # shellcheck disable=SC2016
  bk_report "$report" "$JEST_DEFS"'jest_attempt($cwd; $retry)' --arg cwd "$PWD" --argjson retry "$retry" || status=$?
  case $status in
    0) return ;;
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
  # decides the attempt.
  jq -n --arg reason "$reason" \
    '{messages: ["Buildkite Test Engine Client: Failed to read Jest output, tests will not be retried: \($reason)"]}'
}

# Reads the report of a batch for Test Scheduler pools. Unlike
# interpret-attempt, it rejects a report it can't account for completely.
plugin_parse_batch_report() {
  local format report
  format=$(bk_request .format -r)
  report=$(bk_request .report_path -r)
  [[ $format == jest-json ]] || bk_fail "unsupported runner report format \"$format\""
  # shellcheck disable=SC2016
  bk_report "$report" "$JEST_DEFS"'jest_batch($cwd)' --arg cwd "$PWD" ||
    bk_fail "invalid Jest report: $BK_REPORT_ERROR"
}

bk_main "$@"
