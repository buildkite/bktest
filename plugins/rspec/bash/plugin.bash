#!/usr/bin/env bash
# Bash runner plugin for RSpec.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
RSPEC_DEFS='
# The test command for the paths, or the retry command when $retry.
def rspec_argv($paths; $retry):
  .config.result_path as $result_path
  | (if $retry then .config.retry_test_command else .config.test_command end)
  | bk_words
  | if bk_index("{{testExamples}}") == null then . + $paths
    else bk_splice("{{testExamples}}"; $paths) end
  | bk_splice("{{resultPath}}"; [$result_path // ""]);

# An example of the RSpec JSON report as a test case. The scope and name have
# to match the Buildkite test collector. Its scope is the example group
# description, which the report only has as the full description that ends
# with the description of the example.
def rspec_test_case:
  (.description // "") as $name
  | (.full_description // "") as $full
  | {
      identifier: (.id // ""),
      name: $name,
      path: (.id // ""),
      scope: (if $full | endswith(" " + $name) then $full[:($full | length) - ($name | length) - 1] else $full end)
    };

def rspec_status:
  {passed: "passed", failed: "failed", pending: "skipped"}[. // ""] // "unknown";

def rspec_trim: if startswith("./") then .[2:] else . end;
'

plugin_configure() {
  # Test Engine stores RSpec tests with a leading "./", hence the prefix.
  bk_request '{
    name: "RSpec",
    config: (.config
      | bk_default("test_command"; "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}")
      | bk_default("test_file_pattern"; "spec/**/*_spec.rb")
      | bk_default("retry_test_command"; .test_command)
      | bk_default("location_prefix"; "./")),
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      auto_retry: true, mute: true, skip: true, split_by_selector: true},
    result_format: "rspec-json"
  }'
}

plugin_prepare_attempt() {
  bk_request "$RSPEC_DEFS"'{argv: rspec_argv(bk_paths; .retry)}'
}

# Dry-runs RSpec to list the examples in the files with their runnable IDs.
# The report goes to a file because a custom formatter may own stdout.
plugin_discover_examples() {
  local report="$BK_TMP/dry-run.json" output
  local -a argv
  bk_argv argv "$RSPEC_DEFS"'rspec_argv(.files; false)'
  argv+=(--dry-run --format json --out "$report" --format progress)
  bk_log "Running \`${argv[*]}\` for dry run"
  if ! output=$("${argv[@]}" 2>&1); then
    bk_fail "failed to run rspec dry run: $output"
  fi
  jq "$RSPEC_DEFS"'{test_cases: [.examples[] | rspec_test_case]}' "$report" 2>/dev/null ||
    bk_fail "failed to parse rspec output"
}

# Each example is recorded under its ID without a leading "./", so retries
# target the exact example even when descriptions repeat.
plugin_interpret_attempt() {
  local report
  report=$(bk_cfg result_path)
  local failed='Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried:'
  if [[ ! -r $report ]]; then
    # The command still fails the run with its own error.
    jq -n --arg message "$failed failed to read rspec output: open $report: no such file or directory" '{messages: [$message]}'
    return
  fi
  jq "$RSPEC_DEFS"'{
    tests: [(.examples // [])[] | {
      test_case: rspec_test_case,
      status: (.status | rspec_status),
      key: (.id // "" | rspec_trim)
    }]
  } + (
    (.summary.errors_outside_of_examples_count // 0) as $n
    | if $n > 0 then {run_error: "RSpec reported \($n) errors outside of examples"} else {} end
  )' "$report" 2>/dev/null ||
    jq -n --arg message "$failed failed to parse rspec output" '{messages: [$message]}'
}

# Reads the report of a batch for Test Scheduler pools. Unlike
# interpret-attempt, it rejects an incomplete report, and maps each example
# to the spec file that was run, which is its ID without the [1:2:3] suffix
# and can differ from file_path for shared examples.
plugin_parse_batch_report() {
  local format report
  format=$(bk_request .format -r)
  report=$(bk_request .report_path -r)
  [[ $format == rspec-json ]] || bk_fail "unsupported runner report format \"$format\""
  jq -e . "$report" >/dev/null 2>&1 || bk_fail "failed to parse rspec output"
  # shellcheck disable=SC2016
  jq "$RSPEC_DEFS"'
    if (.examples | type) != "array" or (.summary | type) != "object"
      or (.summary.example_count | type) != "number"
      or .summary.example_count != (.examples | length)
    then error("incomplete RSpec report or mismatched example count") else . end
    | {
        errors_outside_tests: ((.summary.errors_outside_of_examples_count // 0) != 0),
        tests: [.examples[]
          | (.status | rspec_status) as $status
          | if $status == "unknown" then error("unknown RSpec example status \"\(.status // "")\"") else . end
          | (if (.file_path // "") != "" and (.line_number // 0) > 0
             then "\(.file_path | rspec_trim):\(.line_number)" else "" end) as $location
          | (rspec_test_case | .identifier |= rspec_trim | .path |= rspec_trim
             | if .path == "" then .path = $location else . end) as $test_case
          | if $test_case.path == "" then error("RSpec example has no runnable ID or file:line") else . end
          | {
              test_case: $test_case,
              status: $status,
              selector: ((.id // "") as $id
                | (if $id | test("^.+\\[[0-9:]+\\]$") then $id | sub("\\[[0-9:]+\\]$"; "") else (.file_path // "") end)
                | rspec_trim),
              location: $location
            }]
      }' "$report" 2>"$BK_TMP/jq-error" ||
    bk_fail "$(sed -e 's/^jq: error ([^)]*)\( (not a string)\)\{0,1\}: //' "$BK_TMP/jq-error")"
}

bk_main "$@"
