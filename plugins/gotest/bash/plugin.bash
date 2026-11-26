#!/usr/bin/env bash
# Bash runner plugin for go test. It reads the JSON events of `go test -json`
# or `gotestsum --jsonfile`, or otherwise JUnit XML such as
# `gotestsum --junitfile` writes, which is parsed with python3.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
GOTEST_DEFS='
# Whether the input words run `go test -json`, which prints its events on
# stdout.
def gotest_prints_json:
  length >= 3 and (.[0] | sub("/+$"; "") | split("/") | last) == "go"
  and .[1] == "test" and any(.[]; . == "-json");

# The file of the first --jsonfile option in the input words, or "".
def gotest_jsonfile:
  . as $words
  | first(range(length) as $i
      | if $words[$i] == "--jsonfile" and $i + 1 < length then $words[$i + 1]
        elif $words[$i] | startswith("--jsonfile=") then $words[$i][11:]
        else empty end) // "";

# The format of the report: go-jsonl when the test command writes JSON
# events, otherwise junit.
def gotest_format:
  (.config.test_command // "")
  | (try bk_words catch null) as $words
  | if $words == null then (if contains("-json") then "go-jsonl" else "junit" end)
    elif $words | gotest_prints_json or gotest_jsonfile != "" then "go-jsonl"
    else "junit" end;

# The packages of the test cases, each once. A test of a package has the
# package as its path.
def gotest_packages:
  [.test_cases[]
   | (.value // "") as $value
   | if $value != "" and (.format == "selector" or (.path // "") == "") then $value else .path // "" end
   | select(. != "")]
  | [to_entries | group_by(.value)[] | .[0]] | sort_by(.key) | map(.value);

# The test command for the test cases. The packages and the result path are
# put into the command before it is split into words, so a package with a
# space or a quote is not one word.
def gotest_argv:
  "{{resultPath}}" as $placeholder
  | (.config.result_path // "") as $result_path
  | gotest_packages as $packages
  | if ($packages | length) == 0
    then error("failed to generate test package list: unable to extract package names from test plan")
    else . end
  | (bk_test_command // "") as $command
  | ($command | bk_cut("{{packages}}") // [$command + " ", ""]) as [$before, $after]
  | if ($before | contains($placeholder) | not) and any($packages[]; contains($placeholder)) then
      # The first result path placeholder is in a package.
      $before + ($packages | bk_join(" ")) + $after
      | bk_replace_first($placeholder; $result_path)
      | bk_words
    else
      (if $before | contains($placeholder)
       then [($before | bk_replace_first($placeholder; $result_path)), $after]
       else [$before, ($after | bk_replace_first($placeholder; $result_path))] end) as [$prefix, $suffix]
      | bk_words_joined($prefix; $packages; $suffix)
    end;

# A line of the report as an event with a string for every missing
# attribute. Errors are worded as bktest words them for its Go runner.
def gotest_event:
  def text($name):
    .[$name]
    | if . == null then "" elif type == "string" then .
      else error("json: cannot unmarshal \(bk_go_type) into Go struct field goJSONLTestEvent.\($name) of type string") end;
  fromjson
  | if . == null then {}
    elif type != "object" then error("json: cannot unmarshal \(bk_go_type) into Go value of type runner.goJSONLTestEvent")
    else . end
  | {Action: text("Action"), Package: text("Package"), Test: text("Test"), Output: text("Output"), FailedBuild: text("FailedBuild")};

def gotest_status: {pass: "passed", fail: "failed", skip: "skipped"}[.Action];

# Whether the input strings, put together, have the marker go test prints
# for a package that failed to build. Only the end of what came before is
# kept, because adding megabytes of output to a string piece by piece is slow.
def gotest_build_failed:
  reduce (.[] | select(. != "")) as $output ({tail: "", found: false};
    if .found then . else
      (.tail + $output) as $text
      | {tail: $text[-13:], found: ($text | contains("[build failed]"))}
    end)
  | .found;

# The response for the events of a report. A test failed if any of its
# events did. A package that failed to build is an error of the run, which
# retrying cannot fix. A package that failed without a failed test, such as
# one whose TestMain exits, is reported as a failed TestMain to retry.
def gotest_results:
  map(select(.Package != ""))
  | (group_by(.Package)
     | map(.[0].Package as $package
         | map(select(.Test == "" and .Action == "fail")) as $failures
         | select(($failures | length) > 0)
         | {
             package: $package,
             build_failed: (any($failures[]; .FailedBuild != "") or (map(.Output) | gotest_build_failed)),
             failed_test: any(.[]; .Test != "" and .Action == "fail")
           })) as $failed
  | {
      tests: (
        [.[] | select(.Test != "" and gotest_status != null)]
        | group_by(.Package + "/" + .Test)
        | map({
            test_case: (.[-1] | {format: "example", scope: .Package, name: .Test, path: .Package}),
            status: (if any(.[]; .Action == "fail") then "failed" else .[-1] | gotest_status end)
          })
        + [$failed[] | select(.build_failed or .failed_test | not)
           | {test_case: {format: "example", scope: .package, name: "TestMain", path: .package}, status: "failed"}])
    }
    + ([$failed[] | select(.build_failed) | .package] | .[0]
       | if . == null then {} else {run_error: "go test failed to build \(.)"} end);

# gotestsum reports a package that failed to build as a failed TestMain
# without a classname. So it does a TestMain that exits and a timeout, which
# can be retried, so the failure has to have the marker of go test.
def gotest_junit_build_failed:
  .name == "TestMain" and .classname == "" and .failure != null and (.failure | contains("[build failed]"));
'

plugin_configure() {
  # shellcheck disable=SC2016
  bk_request "$GOTEST_DEFS"'
    (.config
      | bk_default("test_command"; "gotestsum --jsonfile={{resultPath}} {{packages}}")
      | bk_default("retry_test_command"; .test_command)) as $config
    | {
        name: "gotest",
        config: $config,
        features: {split_by_selector: true, auto_retry: true, mute: true},
        result_format: ({config: $config} | gotest_format)
      }'
}

# Go has no test files to split by: a package is the smallest unit. Only
# packages with test files are listed, so none takes a place in the plan
# to run nothing.
plugin_discover_targets() {
  local status=0
  # shellcheck disable=SC2016
  bk_log 'Discovering Go packages with tests using `go list`'
  # shellcheck disable=SC2016
  command -v go >/dev/null || bk_fail 'failed to run go list: exec: "go": executable file not found in $PATH'
  go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... \
    >"$BK_TMP/go-list" 2>"$BK_TMP/go-list-error" || status=$?
  if [[ $status -ne 0 ]]; then
    bk_fail "go list failed: exit status $status"$'\n'"stderr:"$'\n'"$(<"$BK_TMP/go-list-error")"
  fi
  # A package without tests is an empty line.
  # shellcheck disable=SC2016
  grep -v '^$' "$BK_TMP/go-list" >"$BK_TMP/packages" || bk_fail 'no Go packages with tests found using `go list`'
  bk_log "Discovered $(wc -l <"$BK_TMP/packages") packages with tests"
  jq -Rn '{targets: [inputs]}' "$BK_TMP/packages"
}

# `go test -json` prints its events on stdout, so bktest writes them to the
# result path for interpret-attempt. gotestsum writes the file itself.
plugin_prepare_attempt() {
  # shellcheck disable=SC2016
  bk_request "$GOTEST_DEFS"'gotest_argv as $argv
    | if gotest_format == "go-jsonl" and ($argv | gotest_prints_json and gotest_jsonfile == "") then
        if (.config.result_path // "") == "" then error("open : no such file or directory") else . end
        | {argv: $argv, stdout_path: .config.result_path}
      else {argv: $argv} end'
}

# go test exits 1 both when tests fail and when a package fails to build.
# After another exit code there is no report worth reading.
plugin_interpret_attempt() {
  local report exit_code reason
  exit_code=$(bk_request '.exit_code' -r)
  if [[ $exit_code != null && $exit_code != 0 && $exit_code != 1 ]]; then
    echo '{}'
    return
  fi

  report=$(bk_cfg result_path)
  if [[ $(bk_request "$GOTEST_DEFS"'gotest_format' -r) == junit ]]; then
    if reason=$(bk_junit_read "$report" "$BK_TMP/junit.json"); then
      # A test of a package has the package as its path, to run it again.
      jq "$GOTEST_DEFS"'{
        tests: [.[] | select(gotest_junit_build_failed | not)
          | {test_case: {format: "example", scope: .classname, name: .name, path: .classname}, status: .result}]
      } + ([.[] | select(gotest_junit_build_failed) | .suite] | last
           | if . == null then {} else {run_error: "go test failed to build \(.)"} end)' "$BK_TMP/junit.json"
      return
    fi
  elif ! reason=$(bk_check_file "$report"); then
    reason="failed to read go jsonl: $reason"
  elif [[ -n $(LC_ALL=C cut -b 1048576- "$report" | tr -d '\n' | head -c 1) ]]; then
    # bktest reads lines of up to a megabyte.
    reason='failed to read go jsonl: bufio.Scanner: token too long'
  elif jq -Rn "$BK_JQ_DEFS $GOTEST_DEFS"'[inputs | select(test("\\S")) | gotest_event]
    | if length == 0 then error("no events found") else gotest_results end' \
    "$report" >"$BK_TMP/tests.json" 2>"$BK_TMP/jq-error"; then
    cat "$BK_TMP/tests.json"
    return
  else
    reason="failed to parse go jsonl: $(bk_jq_error)"
  fi
  # The command still fails the run with its own error.
  jq -n --arg message "Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: $reason" \
    '{messages: [$message]}'
}

bk_main "$@"
