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
    jq -cn --argjson req "$req" '
      ($req.config // {}) as $c |
      ($c.test_command // "" | if . == "" then "npx cypress run --spec {{testExamples}}" else . end) as $test_cmd |
      ($c.retry_test_command // "" | if . == "" then $test_cmd else . end) as $retry_cmd |
      ($c.test_file_pattern // "" | if . == "" then "**/*.cy.{js,jsx,ts,tsx}" else . end) as $pattern |
      {
        protocol_version: 1,
        runner_id: "cypress",
        name: "Cypress",
        config: ($c + {
          test_command: $test_cmd,
          retry_test_command: $retry_cmd,
          test_file_pattern: $pattern
        }),
        features: {
          split_by_file: true,
          split_by_example: false,
          split_by_selector: true,
          filter_test_files: true,
          filter_test_by_tag: false,
          auto_retry: false,
          mute: false,
          skip: false
        },
        capabilities: {
          discover_targets: true,
          discover_examples: false,
          interpret_attempt: false,
          parse_batch_report: false
        }
      }
    '
    ;;

  discover-targets)
    pattern="$(jq -r '.config.test_file_pattern // "**/*.cy.{js,jsx,ts,tsx}"' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    cmd_str="$(jq -r 'if .retry then (.config.retry_test_command // .config.test_command) else .config.test_command end' <<< "$req")"
    if ! words="$(bktest_split_cmd "$cmd_str" 2>&1)"; then
      bktest_err "$words"
    fi
    specs="$(jq -r '[(.test_cases // [])[] | if .format == "selector" then .value else .path end] | join(",")' <<< "$req")"
    jq -cn --argjson words "$words" --arg specs "$specs" '
      ($words | index("{{testExamples}}")) as $idx |
      (if $idx == null then ($words + ["--spec", $specs]) else ($words | .[$idx] = $specs) end) as $final |
      {
        protocol_version: 1,
        command: $final[0],
        args: $final[1:]
      }
    '
    ;;

  interpret-attempt)
    jq -cn '{protocol_version: 1}'
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
