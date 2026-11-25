#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../_lib/common.sh
source "$SCRIPT_DIR/../_lib/common.sh"

op="${1:-}"
req="$(bktest_read_req)"
bktest_chdir_from_req "$req"

build_rspec_words() {
  local req_json="$1"
  local retry_flag="$2"
  local paths_json="$3"
  local cmd_str result_path words

  if [[ "$retry_flag" == "true" ]]; then
    cmd_str="$(jq -r '.config.retry_test_command // .config.test_command' <<< "$req_json")"
  else
    cmd_str="$(jq -r '.config.test_command' <<< "$req_json")"
  fi
  result_path="$(jq -r '.config.result_path // ""' <<< "$req_json")"

  if ! words="$(bktest_split_cmd "$cmd_str" 2>&1)"; then
    bktest_err "$words"
  fi

  jq -cn --argjson words "$words" --argjson paths "$paths_json" --arg rp "$result_path" '
    ($words | index("{{testExamples}}")) as $ti |
    (if $ti == null then ($words + $paths) else ($words[:$ti] + $paths + $words[$ti+1:]) end) as $w1 |
    ($w1 | index("{{resultPath}}")) as $ri |
    (if $ri == null then $w1 else ($w1[:$ri] + [$rp] + $w1[$ri+1:]) end)
  '
}

case "$op" in
  configure)
    jq -cn --argjson req "$req" '
      ($req.config // {}) as $c |
      ($c.test_command // "" | if . == "" then "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}" else . end) as $test_cmd |
      ($c.retry_test_command // "" | if . == "" then $test_cmd else . end) as $retry_cmd |
      ($c.test_file_pattern // "" | if . == "" then "spec/**/*_spec.rb" else . end) as $pattern |
      ($c.location_prefix // "" | if . == "" then "./" else . end) as $loc_prefix |
      {
        protocol_version: 1,
        runner_id: "rspec",
        name: "RSpec",
        config: ($c + {
          test_command: $test_cmd,
          retry_test_command: $retry_cmd,
          test_file_pattern: $pattern,
          location_prefix: $loc_prefix
        }),
        result_format: "rspec-json",
        batch_report_format: "rspec-json",
        requires_result_path: true,
        features: {
          split_by_file: true,
          split_by_example: true,
          split_by_selector: true,
          filter_test_files: true,
          filter_test_by_tag: false,
          auto_retry: true,
          mute: true,
          skip: true
        },
        capabilities: {
          discover_targets: true,
          discover_examples: true,
          interpret_attempt: true,
          parse_batch_report: true
        }
      }
    '
    ;;

  discover-targets)
    pattern="$(jq -r '.config.test_file_pattern // "spec/**/*_spec.rb"' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    retry="$(jq -r '.retry // false' <<< "$req")"
    paths_json="$(jq -c '[(.test_cases // [])[] | if .format == "selector" then .value else .path end]' <<< "$req")"
    if ! final_words="$(build_rspec_words "$req" "$retry" "$paths_json")"; then
      printf '%s\n' "$final_words"
      exit 1
    fi
    jq -cn --argjson w "$final_words" '{protocol_version: 1, command: $w[0], args: $w[1:]}'
    ;;

  discover-examples)
    files_json="$(jq -c '.files // []' <<< "$req")"
    tmp_file="$(mktemp /tmp/dry-run-XXXXXX.json)"
    trap 'rm -f "$tmp_file"' EXIT

    if ! final_words="$(build_rspec_words "$req" "false" "$files_json")"; then
      printf '%s\n' "$final_words"
      exit 1
    fi
    mapfile -t cmd_arr < <(jq -r '.[]' <<< "$final_words")
    cmd_arr+=(--dry-run --format json --out "$tmp_file" --format progress)

    if ! output="$("${cmd_arr[@]}" 2>&1)"; then
      bktest_err "failed to run rspec dry run: $output"
    fi

    if ! jq -e . "$tmp_file" >/dev/null 2>&1; then
      bktest_err "failed to parse rspec output"
    fi

    jq -c '
      {
        protocol_version: 1,
        examples: [
          (.examples // [])[] |
          (.description // "") as $desc |
          (.full_description // "") as $full |
          ($full | if endswith(" " + $desc) then .[:-($desc | length) - 1] else . end) as $scope |
          {
            identifier: (.id // ""),
            name: $desc,
            path: (.id // ""),
            scope: $scope
          }
        ]
      }
    ' "$tmp_file"
    ;;

  interpret-attempt)
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    if [[ ! -f "$result_path" ]]; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried: failed to read rspec output: open ${result_path}: no such file or directory" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi
    if ! parsed="$(jq -c '
      def map_status:
        if . == "passed" then "passed"
        elif . == "failed" then "failed"
        elif . == "pending" then "skipped"
        else "unknown"
        end;
      {
        protocol_version: 1,
        results: [
          (.examples // [])[] |
          (.description // "") as $desc |
          (.full_description // "") as $full |
          ($full | if endswith(" " + $desc) then .[:-($desc | length) - 1] else . end) as $scope |
          (.id // "") as $id |
          ($id | ltrimstr("./")) as $trimmed_id |
          {
            key: (if $trimmed_id != "" then $trimmed_id else ($scope + "/" + $desc + "/" + $id) end),
            test_case: {
              identifier: $id,
              name: $desc,
              path: $id,
              scope: $scope
            },
            status: ((.status // "") | map_status)
          }
        ]
      } + (
        ((.summary.errors_outside_of_examples_count // 0) | tonumber) as $ec |
        if $ec > 0 then {run_error: ("RSpec reported " + ($ec | tostring) + " errors outside of examples")} else {} end
      )
    ' "$result_path" 2>&1)"; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried: failed to parse rspec output: ${parsed}" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi
    printf '%s\n' "$parsed"
    ;;

  parse-batch-report)
    fmt="$(jq -r '.format // ""' <<< "$req")"
    if [[ "$fmt" != "rspec-json" ]]; then
      bktest_err "unsupported runner report format \"$fmt\""
    fi
    raw="$(jq -r '.report_base64 // ""' <<< "$req" | base64 -d)"
    if ! jq -e '(.examples | type == "array") and (.summary | type == "object") and (.summary | has("example_count")) and ((.summary.example_count | type) == "number") and (.summary.example_count == (.examples | length))' <<< "$raw" >/dev/null 2>&1; then
      bktest_err "incomplete RSpec report or mismatched example count"
    fi
    if ! out="$(jq -c '
      def map_status:
        if . == "passed" then "passed"
        elif . == "failed" then "failed"
        elif . == "pending" then "skipped"
        else error("unknown RSpec example status \"" + . + "\"")
        end;
      def primary_selector:
        (.id // "") as $id |
        (.file_path // "") as $fp |
        if ($id | test("^.+\\[[0-9:]+\\]$")) then
          ($id | sub("\\[[0-9:]+\\]$"; ""))
        else
          $fp
        end;
      {
        protocol_version: 1,
        errors_outside_tests: ((.summary.errors_outside_of_examples_count // 0) != 0),
        tests: [
          .examples[] |
          ((.status // "") | map_status) as $st |
          (.description // "") as $desc |
          (.full_description // "") as $full |
          ($full | if endswith(" " + $desc) then .[:-($desc | length) - 1] else . end) as $scope |
          ((.id // "") | ltrimstr("./")) as $id |
          ((.file_path // "") | ltrimstr("./")) as $fp |
          (.line_number // 0) as $ln |
          (if ($fp != "" and $ln > 0) then ($fp + ":" + ($ln | tostring)) else "" end) as $loc |
          (if $id != "" then $id else $loc end) as $path |
          if $path == "" then error("RSpec example has no runnable ID or file:line") else
          {
            test_case: {
              identifier: $id,
              name: $desc,
              path: $path,
              scope: $scope
            },
            status: $st,
            selector: (primary_selector | ltrimstr("./")),
            location: $loc
          } end
        ]
      }
    ' <<< "$raw" 2>&1)"; then
      err_msg="$(sed 's/^jq: error (at <stdin>:[0-9]*): //' <<< "$out")"
      bktest_err "$err_msg"
    fi
    printf '%s\n' "$out"
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
