#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../_lib/common.sh
source "$SCRIPT_DIR/../_lib/common.sh"

op="${1:-}"
req="$(bktest_read_req)"
bktest_chdir_from_req "$req"

build_cucumber_words() {
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
      ($c.test_command // "" | if . == "" then "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}" else . end) as $test_cmd |
      ($c.retry_test_command // "" | if . == "" then $test_cmd else . end) as $retry_cmd |
      ($c.test_file_pattern // "" | if . == "" then "features/**/*.feature" else . end) as $pattern |
      {
        protocol_version: 1,
        runner_id: "cucumber",
        name: "Cucumber",
        config: ($c + {
          test_command: $test_cmd,
          retry_test_command: $retry_cmd,
          test_file_pattern: $pattern
        }),
        result_format: "cucumber-json",
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
          parse_batch_report: false
        }
      }
    '
    ;;

  discover-targets)
    pattern="$(jq -r '.config.test_file_pattern // "features/**/*.feature"' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    retry="$(jq -r '.retry // false' <<< "$req")"
    paths_json="$(jq -c '[(.test_cases // [])[] | if .format == "selector" then .value else .path end]' <<< "$req")"
    if ! final_words="$(build_cucumber_words "$req" "$retry" "$paths_json")"; then
      printf '%s\n' "$final_words"
      exit 1
    fi
    jq -cn --argjson w "$final_words" '{protocol_version: 1, command: $w[0], args: $w[1:]}'
    ;;

  discover-examples)
    file_count="$(jq '(.files // []) | length' <<< "$req")"
    if [[ "$file_count" -eq 0 ]]; then
      jq -cn '{protocol_version: 1, examples: []}'
      exit 0
    fi

    tmp_file="$(mktemp /tmp/cucumber-dry-run-XXXXXX.json)"
    trap 'rm -f "$tmp_file"' EXIT

    files_json="$(jq -c '.files // []' <<< "$req")"
    if ! final_words="$(build_cucumber_words "$req" "false" "$files_json")"; then
      printf '%s\n' "$final_words"
      exit 1
    fi
    cmd_name="$(jq -r '.[0]' <<< "$final_words")"
    mapfile -t files_arr < <(jq -r '.[]' <<< "$files_json")

    if ! output="$("$cmd_name" --dry-run --format json --out "$tmp_file" --format progress "${files_arr[@]}" 2>&1)"; then
      bktest_err "failed to run Cucumber dry run: $output"
    fi

    if [[ ! -s "$tmp_file" ]]; then
      jq -cn '{protocol_version: 1, examples: []}'
      exit 0
    fi

    jq -c '
      (if type == "array" then . else [.] end) as $features |
      {
        protocol_version: 1,
        examples: [
          $features[] |
          (.uri // "") as $uri |
          (.name // "") as $fname |
          (.elements // [])[] |
          select(.type == "scenario") |
          ($uri + ":" + ((.line // 0) | tostring)) as $id |
          {
            path: $id,
            scope: $fname,
            name: (.name // ""),
            identifier: $id
          }
        ]
      }
    ' "$tmp_file"
    ;;

  interpret-attempt)
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    if [[ ! -f "$result_path" ]]; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: failed to read cucumber output: open ${result_path}: no such file or directory" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi
    if ! parsed="$(jq -c '
      def agg_status:
        reduce (.steps // [])[] as $s ("passed";
          if . == "failed" then "failed"
          elif ($s.result == null) then .
          elif ($s.result.status == "failed" or $s.result.status == "undefined" or $s.result.status == "errored") then "failed"
          elif ($s.result.status == "pending" or $s.result.status == "skipped") then "pending"
          else .
          end
        );
      def map_status:
        if (. == "failed" or . == "undefined" or . == "errored") then "failed"
        elif . == "passed" then "passed"
        elif (. == "pending" or . == "skipped") then "skipped"
        else "unknown"
        end;
      {
        protocol_version: 1,
        results: [
          .[] |
          (.uri // "") as $uri |
          (.name // "") as $fname |
          (.elements // [])[] |
          select(.type == "scenario") |
          ($uri + ":" + ((.line // 0) | tostring)) as $id |
          {
            test_case: {
              identifier: $id,
              name: (.name // ""),
              scope: $fname,
              path: $id
            },
            status: (agg_status | map_status)
          }
        ]
      }
    ' "$result_path" 2>&1)"; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: failed to parse cucumber output: ${parsed}" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi
    printf '%s\n' "$parsed"
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
