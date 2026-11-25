#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../_lib/common.sh
source "$SCRIPT_DIR/../_lib/common.sh"

op="${1:-}"
req="$(bktest_read_req)"
bktest_chdir_from_req "$req"

case "$op" in
  configure)
    test_cmd="$(jq -r '.config.test_command // ""' <<< "$req")"
    pattern="$(jq -r '.config.test_file_pattern // ""' <<< "$req")"
    sel_path="$(jq -r '.config.selector_list_path // ""' <<< "$req")"
    if [[ -z "$test_cmd" ]]; then
      bktest_err "test command must be provided for custom runner"
    fi
    if [[ -z "$pattern" && -z "$sel_path" ]]; then
      bktest_err "test file pattern must be provided for custom runner"
    fi
    jq -cn --argjson req "$req" '
      ($req.config // {}) as $c |
      ($c.test_command // "") as $test_cmd |
      ($c.retry_test_command // "" | if . == "" then $test_cmd else . end) as $retry_cmd |
      ($c.result_path // "") as $rp |
      (if $rp == "" then "" elif ($rp | endswith(".xml")) then "junit" else "json" end) as $rf |
      {
        protocol_version: 1,
        runner_id: "custom",
        name: "Custom test runner",
        config: ($c + {
          retry_test_command: $retry_cmd
        }),
        result_format: $rf,
        features: {
          split_by_file: true,
          split_by_example: false,
          split_by_selector: true,
          filter_test_files: true,
          filter_test_by_tag: false,
          auto_retry: false,
          mute: true,
          skip: false
        },
        capabilities: {
          discover_targets: true,
          discover_examples: false,
          interpret_attempt: ($rp != ""),
          parse_batch_report: false
        }
      }
    '
    ;;

  discover-targets)
    echo "Buildkite Test Engine Client: --selector-file (or BUILDKITE_TEST_ENGINE_SELECTOR_FILE) is not set for the custom runner. Discovering selector values with the configured test file pattern." >&2
    pattern="$(jq -r '.config.test_file_pattern // ""' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    cmd_str="$(jq -r 'if .retry then (.config.retry_test_command // .config.test_command) else .config.test_command end' <<< "$req")"
    joined_paths="$(jq -r '[(.test_cases // [])[] | if .format == "selector" then .value else .path end] | join(" ")' <<< "$req")"
    cmd_replaced="${cmd_str/\{\{testExamples\}\}/$joined_paths}"
    if ! words="$(bktest_split_cmd "$cmd_replaced" 2>&1)"; then
      bktest_err "$words"
    fi
    jq -cn --argjson w "$words" '{protocol_version: 1, command: $w[0], args: $w[1:]}'
    ;;

  interpret-attempt)
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    if [[ -z "$result_path" ]]; then
      jq -cn '{protocol_version: 1}'
      exit 0
    fi
    if [[ "$result_path" == *.xml ]]; then
      if ! junit_json="$(bktest_junit_to_json "$result_path" 2>&1)"; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read JUnit XML output: ${junit_json}" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      jq -cn --argjson tests "$junit_json" '
        {
          protocol_version: 1,
          results: [
            $tests[] | {
              test_case: {
                format: "example",
                scope: .classname,
                name: .name,
                path: .classname
              },
              status: .result
            }
          ]
        }
      '
    else
      if [[ ! -f "$result_path" ]]; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read test engine JSON output: failed to read json: open ${result_path}: no such file or directory" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      if ! parsed="$(jq -c '
        {
          protocol_version: 1,
          results: [
            .[] | {
              test_case: {
                identifier: (.id // ""),
                format: "example",
                scope: (.scope // ""),
                name: (.name // ""),
                path: ((.file_name // "") + ":" + (.location // ""))
              },
              status: (.result // "unknown")
            }
          ]
        }
      ' "$result_path" 2>&1)"; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read test engine JSON output: failed to parse json: ${parsed}" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      printf '%s\n' "$parsed"
    fi
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
