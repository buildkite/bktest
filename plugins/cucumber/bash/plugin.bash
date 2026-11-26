#!/usr/bin/env bash
# Bash runner plugin for Cucumber (Ruby). It reads the report of Cucumber's
# JSON formatter, so a custom test command has to include
# `--format json --out {{resultPath}}`.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
CUCUMBER_DEFS='
# The test command for the paths, or the retry command when the request is a
# retry.
def cucumber_argv($paths):
  .config.result_path as $result_path
  | bk_test_command | bk_words
  | if bk_index("{{testExamples}}") == null then . + $paths
    else bk_splice("{{testExamples}}"; $paths) end
  | bk_splice("{{resultPath}}"; [$result_path // ""]);

# The input, which is null or of the JSON type $json_type. Otherwise the
# error is worded as bktest words it for its Go runner, with the path of the
# field and the Go type it expects.
def cucumber_expect($json_type; field; $go_type):
  if . == null or type == $json_type then .
  else error("json: cannot unmarshal \(bk_go_type) into Go struct field \(field) of type \($go_type)") end;

# The same for an item of an array, which is an object.
def cucumber_item(field; $go_type):
  if . == null or type == "object" then .
  else error("json: cannot unmarshal \(bk_go_type) into \(field) of type \($go_type)") end;

# The scenarios of a report, which is an array of features, as {test_case,
# status}. A scenario is a test, with its file and line as its path. It
# failed if a step failed or is undefined, was skipped if a step is pending
# or was skipped, and otherwise passed, as it does in a dry run, where steps
# have no result.
def cucumber_scenarios:
  if . == null then []
  elif type != "array" then error("json: cannot unmarshal \(bk_go_type) into Go value of type []runner.CucumberFeature")
  else . end
  | range(length) as $i
  | .[$i]
  | cucumber_item(".\($i)"; "runner.CucumberFeature")
  | (.uri | cucumber_expect("string"; ".\($i).uri"; "string") // "") as $uri
  | (.name | cucumber_expect("string"; ".\($i).name"; "string") // "") as $feature
  | (.elements | cucumber_expect("array"; ".\($i).elements"; "[]runner.CucumberElement") // [])
  | range(length) as $j
  | .[$j]
  | cucumber_item(".\($i).elements.\($j)"; "runner.CucumberElement")
  | [(.steps | cucumber_expect("array"; ".\($i).elements.\($j).steps"; "[]runner.CucumberStep") // [])
     | range(length) as $k
     | .[$k]
     | cucumber_item(".\($i).elements.\($j).steps.\($k)"; "runner.CucumberStep")
     | .result
     | cucumber_expect("object"; ".\($i).elements.\($j).steps.\($k).result"; "runner.CucumberResult")
     | .status
     | cucumber_expect("string"; ".\($i).elements.\($j).steps.\($k).result.status"; "string") // ""] as $statuses
  | (.line | cucumber_expect("number"; ".\($i).elements.\($j).line"; "int") // 0
     | if . != floor then error("json: cannot unmarshal number \(.) into Go struct field .\($i).elements.\($j).line of type int") else . end) as $line
  | (.name | cucumber_expect("string"; ".\($i).elements.\($j).name"; "string") // "") as $name
  | select((.type | cucumber_expect("string"; ".\($i).elements.\($j).type"; "string")) == "scenario")
  | "\($uri):\($line)" as $location
  | {
      test_case: {identifier: $location, name: $name, scope: $feature, path: $location},
      status: (
        if any($statuses[]; . == "failed" or . == "undefined" or . == "errored") then "failed"
        elif any($statuses[]; . == "pending" or . == "skipped") then "skipped"
        else "passed" end)
    };
'

plugin_configure() {
  # The pretty formatter shows progress. The JSON formatter is for bktest.
  bk_request '{
    name: "Cucumber",
    config: (.config
      | bk_default("test_command"; "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}")
      | bk_default("test_file_pattern"; "features/**/*.feature")
      | bk_default("retry_test_command"; .test_command)),
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      auto_retry: true, mute: true, skip: true, split_by_selector: true},
    result_format: "cucumber-json"
  }'
}

plugin_prepare_attempt() {
  bk_request "$CUCUMBER_DEFS"'{argv: cucumber_argv(bk_paths)}'
}

# Dry-runs Cucumber, with the executable of the test command, to list the
# scenarios in the files. A scenario outline is listed as the scenarios of
# its examples.
plugin_discover_examples() {
  local report="$BK_TMP/dry-run.json"
  local -a argv files
  bk_argv files '.files // []'
  if [[ ${#files[@]} -eq 0 ]]; then
    echo '{"test_cases": []}'
    return
  fi
  bk_argv argv "$CUCUMBER_DEFS"'cucumber_argv(.files) | .[:1]'
  argv+=(--dry-run --format json --out "$report" --format progress)
  bk_log "Running \`${argv[*]} ${files[*]}\` for dry run"
  : >"$report"
  if ! "${argv[@]}" "${files[@]}" >"$BK_TMP/dry-run-output" 2>&1; then
    bk_fail "failed to run Cucumber dry run: $(<"$BK_TMP/dry-run-output")"
  fi
  # No report means no scenarios. One feature may be reported on its own.
  if [[ ! -s $report ]]; then
    echo '{"test_cases": []}'
    return
  fi
  jq -n "$BK_JQ_DEFS $CUCUMBER_DEFS"'bk_document | if type == "object" then [.] else . end
    | {test_cases: [cucumber_scenarios | .test_case]}' "$report" 2>"$BK_TMP/jq-error" ||
    bk_fail "failed to parse cucumber dry run JSON report from $report: $(bk_jq_error)"
}

# Cucumber exits with an error when scenarios fail, so the report is read
# whatever the exit status.
plugin_interpret_attempt() {
  local report reason
  report=$(bk_cfg result_path)
  if ! reason=$(bk_check_file "$report"); then
    reason="failed to read cucumber output: $reason"
  elif jq -n "$BK_JQ_DEFS $CUCUMBER_DEFS"'bk_document | {tests: [cucumber_scenarios]}' \
    "$report" >"$BK_TMP/tests.json" 2>"$BK_TMP/jq-error"; then
    cat "$BK_TMP/tests.json"
    return
  else
    reason="failed to parse cucumber output: $(bk_jq_error)"
  fi
  # The command still fails the run with its own error.
  jq -n --arg message "Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: $reason" \
    '{messages: [$message]}'
}

bk_main "$@"
