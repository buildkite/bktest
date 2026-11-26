#!/usr/bin/env bash
# Bash runner plugin for the custom runner: any test command that takes
# files. It reads a JUnit XML report, parsed with python3, when the result
# path ends in ".xml", and Test Engine JSON for any other result path.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
CUSTOM_DEFS='
# The test command for the test cases. The files are put into the command,
# with spaces between them, before it is split into words. So a file with a
# space is more than one word, and the placeholder can be inside quotes. A
# command without the placeholder runs as it is.
def custom_argv:
  (bk_test_command // "") as $command
  | (bk_paths | map(. // "")) as $paths
  | ($command | bk_cut("{{testExamples}}")) as $parts
  | if $parts == null then $command | bk_words
    else bk_words_joined($parts[0]; $paths; $parts[1]) end;
'

# There are no defaults for the test command and the files. A selector list
# gives the files instead of the pattern.
plugin_configure() {
  bk_request '
    if (.config.test_command // "") == ""
    then error("test command must be provided for custom runner")
    elif (.config.test_file_pattern // "") == "" and (.config.selector_list_path // "") == ""
    then error("test file pattern must be provided for custom runner")
    else . end
    | {
        name: "Custom test runner",
        config: (.config | bk_default("retry_test_command"; .test_command)),
        features: {split_by_file: true, filter_test_files: true, mute: true, split_by_selector: true},
        result_format: (.config.result_path // ""
          | if . == "" then "" elif endswith(".xml") then "junit" else "json" end)
      }'
}

# bktest finds the files with the patterns. This is only called to split by
# selector without a selector list, which the notice explains.
plugin_discover_targets() {
  echo 'Buildkite Test Engine Client: --selector-file (or BUILDKITE_TEST_ENGINE_SELECTOR_FILE) is not set for the custom runner. Discovering selector values with the configured test file pattern.' >&2
  bk_request '{pattern: (.config.test_file_pattern // ""), exclude_pattern: (.config.test_file_exclude_pattern // "")}
    | if .pattern == ""
      then error("no files found with pattern \"\" and exclude pattern \(.exclude_pattern | tojson)")
      else . end'
}

plugin_prepare_attempt() {
  bk_request "$CUSTOM_DEFS"'{argv: custom_argv}'
}

# Without a result path only the exit status of the command counts. The
# tests of a JUnit XML report have their classname as their path.
plugin_interpret_attempt() {
  local report reason
  report=$(bk_cfg result_path)
  if [[ -z $report ]]; then
    echo '{}'
    return
  fi

  if [[ $report == *.xml ]]; then
    if reason=$(bk_junit_read "$report" "$BK_TMP/junit.json"); then
      jq '{tests: map({
        test_case: {format: "example", scope: .classname, name: .name, path: .classname},
        status: .result
      })}' "$BK_TMP/junit.json"
      return
    fi
    reason="Failed to read JUnit XML output: $reason"
  elif ! reason=$(bk_check_file "$report"); then
    reason="Failed to read test engine JSON output: failed to read json: $reason"
  elif jq -n "$BK_JQ_DEFS"'bk_document | bk_test_engine_tests | {tests: map({
      test_case: {identifier: .id, format: "example", scope: .scope, name: .name, path: "\(.file_name):\(.location)"},
      status: .result
    })}' "$report" >"$BK_TMP/tests.json" 2>"$BK_TMP/jq-error"; then
    cat "$BK_TMP/tests.json"
    return
  else
    reason="Failed to read test engine JSON output: failed to parse json: $(bk_jq_error)"
  fi
  # The command still decides how the run ends.
  jq -n --arg message "Buildkite Test Engine Client: $reason" '{messages: [$message]}'
}

bk_main "$@"
