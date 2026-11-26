#!/usr/bin/env bash
# Bash runner plugin for Playwright.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
PLAYWRIGHT_DEFS='
# The test command, or the retry command when the request is a retry, with
# the $paths to run.
def pw_argv($paths):
  bk_test_command | bk_words
  | if bk_index("{{testExamples}}") == null then . + $paths
    else bk_splice("{{testExamples}}"; $paths) end;

# The specs of the input suite and of the suites inside it, in report order,
# each with the titles of its suites joined by spaces, from $titles down.
def pw_specs($titles):
  (.specs | bk_array | .[] | {spec: ., titles: $titles}),
  (.suites | bk_array | .[] | pw_specs("\($titles) \(.title | bk_string)"));

# The specs of the input report.
def pw_report_specs: .suites | bk_array | .[] | pw_specs(.title | bk_string);

# A spec from pw_specs as a test case. A spec is a test at a file and line,
# which runs once in each project. The scope has to match the Buildkite test
# collector for Playwright, which joins the titles from the root suite down
# to the test with spaces: the root has no title, then come the project, the
# file, the describe blocks and the test.
def pw_test_case:
  (.spec.title | bk_string) as $title
  | {
      name: $title,
      path: "\(.spec.file | bk_string):\(.spec.line // 0)",
      scope: " \(.spec.tests | bk_array | .[0].projectName | bk_string) \(.titles) \($title)"
    };

# The status of the input spec. A test that was not meant to be skipped but
# was, such as after a failure earlier in its serial group, did not run, and
# a passing retry of another test must not hide that.
def pw_status:
  (.tests | bk_array) as $runs
  | if .ok != true then "failed"
    elif any($runs[]; .status == "skipped" and .expectedStatus != "skipped") then "not_run"
    elif $runs[0].status == "skipped" then "skipped"
    else "passed" end;

# The input array without repeated values, in the order they first appear.
def pw_unique:
  [range(0; length) as $index | {index: $index, value: .[$index]}]
  | group_by(.value) | map(.[0]) | sort_by(.index) | map(.value);

# The files, relative to the directory $cwd, that the input errors of a
# report are located in, such as test files that failed to load. It is null
# if an error has no absolute file.
def pw_error_files($cwd):
  map(.location.file | bk_string)
  | if all(.[]; startswith("/")) then map(bk_rel($cwd)) | pw_unique else null end;

# What the report says about an attempt, for the interpret-attempt response.
#
# Playwright runs no tests at all when a test file fails to load, and
# locates the error in that file. When every error is in a file the command
# was given, those files errored and the other test cases of $request run
# again. Any other error, such as one in a global setup, errors the run.
def pw_attempt($cwd; $request):
  (.suites | bk_array) as $suites
  | (.errors | bk_array) as $errors
  | {tests: [pw_report_specs | {test_case: pw_test_case, status: (.spec | pw_status)}]}
  + if ($errors | length) == 0 then {}
    else
      ($errors | pw_error_files($cwd)) as $files
      # The file of each test case: what Playwright runs, without a line.
      | [$request.test_cases[]
         | (if .format == "selector" then .value else .path end) // ""
         | sub(":[0-9]+\\z"; "") | ltrimstr("./")] as $case_files
      | if $files == null or ($suites | length) > 0 or ($files - $case_files | length) > 0
        then {run_error: "Playwright failed with errors"}
        else
          ($files | map({(.): true}) | add) as $errored
          | [range(0; $case_files | length) | select($errored[$case_files[.]] != true) | $request.test_cases[.]] as $rerun
          | {errored_files: $files}
            + if ($rerun | length) == 0 then {}
              else {
                messages: ["+++ Buildkite Test Engine Client: Playwright failed to load \($files | join(", ")). Running the other test files again."],
                rerun: $rerun
              } end
        end
    end;

# The input test runs with those of the same file and scope merged into the
# first of them. A test repeated in one project, such as with --repeat-each,
# is reported once: failed if any run failed, and skipped only if all were.
def pw_merge_repeated:
  [range(0; length) as $index | {index: $index, test: .[$index]}]
  | group_by(.test | [.selector, .test_case.scope])
  | map(.[0] + {
      status: (reduce .[1:][].test.status as $status (.[0].test.status;
        if $status == "failed" or . == "skipped" then $status else . end))
    })
  | sort_by(.index)
  | map({test_case: .test.test_case, status: .status, selector: .test.selector});

# The report of a batch, for the parse-batch-report response. Each run of a
# test in a project is a test, with the project in its scope like the
# Buildkite test collector, so muting stays per project. Spec files are
# relative to the root directory of the report, and selectors to $cwd.
def pw_batch($cwd):
  (.config.rootDir | bk_string) as $root
  | if .suites == null or $root == ""
    then error("incomplete Playwright report: missing suites or config.rootDir") else . end
  | [pw_report_specs
     | .titles as $titles
     | .spec
     | (.title | bk_string) as $title
     | if (.file | bk_string) == "" then error("playwright test \"\($title)\" has no file") else . end
     | ("\($root)/\(.file)" | bk_rel($cwd)) as $path
     | (.line // 0) as $line
     | .tests | bk_array | .[]
     | (.status | bk_string) as $status
     | {
         test_case: {
           scope: " \(.projectName | bk_string) \($titles) \($title)",
           name: $title,
           path: "\($path):\($line)"
         },
         status: ({expected: "passed", flaky: "passed", unexpected: "failed", skipped: "skipped"}[$status]
           // error("unknown Playwright test status \"\($status)\"")),
         selector: $path,
         not_run: ($status == "skipped" and .expectedStatus != "skipped")
       }] as $runs
  | ($runs | pw_merge_repeated) as $tests
  | (.errors | bk_array) as $errors
  | ($errors | pw_error_files($cwd)) as $error_files
  # Playwright runs the tests of setup projects with every command.
  | {tests: $tests, unselected_tests: true}
  + if ($errors | length) > 0 and $error_files != null and ($tests | length) == 0
    # No test ran because these files failed to load.
    then {errored_files: $error_files, errored_files_stopped_run: true}
    # Other errors outside tests do not say which tests they affect. The
    # files with tests that did not run failed outside their tests.
    else {
      errored_files: ([$runs[] | select(.not_run) | .selector] | pw_unique),
      errors_outside_tests: (($errors | length) > 0)
    } end;
'

# Skip stays off: the same test runs in several projects as examples with the
# same file and line, and that is all Playwright is given, so skipping the
# example of one project would still run it.
plugin_configure() {
  bk_request '{
    name: "Playwright",
    config: (.config
      | bk_default("test_command"; "npx playwright test")
      | bk_default("retry_test_command"; .test_command)
      | bk_default("test_file_pattern"; "**/{*.spec,*.test}.{ts,js}")),
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      auto_retry: true, mute: true, split_by_selector: true}
  } + (if (.config.result_path // "") != "" then {result_format: "playwright-json"} else {} end)'
}

plugin_prepare_attempt() {
  bk_request "$PLAYWRIGHT_DEFS"'{argv: pw_argv(bk_paths | map(. // ""))}'
}

# Lists the tests in the files with --list. The report goes to a file, whose
# path Playwright only takes from the environment, because package managers
# such as yarn add their own output to stdout.
plugin_discover_examples() {
  local report="$BK_TMP/list.json" status=0
  local -a argv
  if [[ $(bk_request '.files | length') -eq 0 ]]; then
    echo '{"test_cases": []}'
    return
  fi
  bk_argv argv "$PLAYWRIGHT_DEFS"'pw_argv(.files)'
  argv+=(--list --reporter=json)
  bk_log "Running \`${argv[*]}\` to list tests"
  : >"$report"
  PLAYWRIGHT_JSON_OUTPUT_NAME=$report "${argv[@]}" >/dev/null 2>"$BK_TMP/list-stderr" || status=$?
  if [[ $status -ne 0 ]]; then
    bk_fail "failed to list playwright tests: exit status $status"$'\n'"stderr: $(<"$BK_TMP/list-stderr")"
  fi
  bk_report "$report" "$PLAYWRIGHT_DEFS"'{test_cases: [pw_report_specs | pw_test_case]}' ||
    bk_fail "failed to parse playwright test list output: failed to parse playwright output: $BK_REPORT_ERROR"
}

# Playwright exits with a failure when tests fail, so the report is read
# whatever the command's exit status was.
plugin_interpret_attempt() {
  local report reason status=0
  report=$(bk_cfg result_path)
  # shellcheck disable=SC2016
  bk_report "$report" "$PLAYWRIGHT_DEFS"'pw_attempt($cwd; $request[0])' \
    --arg cwd "$PWD" --slurpfile request "$BK_REQUEST" || status=$?
  case $status in
    0) return ;;
    1) reason="failed to read playwright output: $BK_REPORT_ERROR" ;;
    *) reason="failed to parse playwright output: $BK_REPORT_ERROR" ;;
  esac
  # Without a report nothing is retried, and the command's own exit status
  # decides the attempt.
  jq -n --arg reason "$reason" \
    '{messages: ["Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: \($reason)"]}'
}

# Reads the report of a batch for Test Scheduler pools. Unlike
# interpret-attempt, it rejects a report it can't account for completely.
plugin_parse_batch_report() {
  local format report
  format=$(bk_request .format -r)
  report=$(bk_request .report_path -r)
  [[ $format == playwright-json ]] || bk_fail "unsupported runner report format \"$format\""
  # shellcheck disable=SC2016
  bk_report "$report" "$PLAYWRIGHT_DEFS"'pw_batch($cwd)' --arg cwd "$PWD" ||
    bk_fail "invalid Playwright report: $BK_REPORT_ERROR"
}

bk_main "$@"
