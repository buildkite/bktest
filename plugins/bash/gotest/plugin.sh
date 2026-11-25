#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../_lib/common.sh
source "$SCRIPT_DIR/../_lib/common.sh"

op="${1:-}"
req="$(bktest_read_req)"
bktest_chdir_from_req "$req"

# Detects whether a tokenized command array produces go-jsonl (either
# `go test ... -json ...` or `--jsonfile`).
detect_go_jsonl_from_words() {
  local words_json="$1"
  jq -r '
    def is_go_test_json:
      (length >= 3) and
      ((.[0] | split("/") | last) == "go") and
      (.[1] == "test") and
      (any(.[]; . == "-json"));
    def jsonfile_arg:
      [range(0; length) as $i |
        if .[$i] == "--jsonfile" and ($i + 1) < length then .[$i+1]
        elif (.[$i] | startswith("--jsonfile=")) then (.[$i] | ltrimstr("--jsonfile="))
        else empty end
      ] | first // "";
    if (is_go_test_json or (jsonfile_arg != "")) then "go-jsonl" else "junit" end
  ' <<< "$words_json"
}

detect_result_format() {
  local cmd_str="$1"
  local words
  if words="$(bktest_split_cmd "$cmd_str" 2>/dev/null)"; then
    detect_go_jsonl_from_words "$words"
  else
    if [[ "$cmd_str" == *"--jsonfile"* || "$cmd_str" == *"-json"* ]]; then
      echo "go-jsonl"
    else
      echo "junit"
    fi
  fi
}

case "$op" in
  configure)
    test_cmd="$(jq -r '.config.test_command // ""' <<< "$req")"
    if [[ -z "$test_cmd" ]]; then
      test_cmd="gotestsum --jsonfile={{resultPath}} {{packages}}"
    fi
    result_format="$(detect_result_format "$test_cmd")"
    jq -cn --argjson req "$req" --arg tc "$test_cmd" --arg rf "$result_format" '
      ($req.config // {}) as $c |
      ($c.retry_test_command // "" | if . == "" then $tc else . end) as $rc |
      {
        protocol_version: 1,
        runner_id: "gotest",
        name: "gotest",
        config: ($c + {
          test_command: $tc,
          retry_test_command: $rc
        }),
        result_format: $rf,
        requires_result_path: true,
        features: {
          split_by_file: false,
          split_by_example: false,
          split_by_selector: true,
          filter_test_files: false,
          filter_test_by_tag: false,
          auto_retry: true,
          mute: true,
          skip: false
        },
        capabilities: {
          discover_targets: true,
          discover_examples: false,
          interpret_attempt: true,
          parse_batch_report: false
        },
        state: {
          result_format: $rf
        }
      }
    '
    ;;

  discover-targets)
    stderr_file="$(mktemp /tmp/gotest-list-stderr-XXXXXX)"
    trap 'rm -f "$stderr_file"' EXIT
    if ! out="$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... 2>"$stderr_file")"; then
      err_txt="$(cat "$stderr_file")"
      bktest_err "go list failed: stderr:
$err_txt"
    fi
    targets="$(printf '%s\n' "$out" | jq -Rsc 'split("\n") | map(select(. != ""))')"
    if [[ "$(jq 'length' <<< "$targets")" -eq 0 ]]; then
      bktest_err 'no Go packages with tests found using `go list`'
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    pkgs_json="$(jq -c '
      reduce (.test_cases // [])[] as $tc ({seen: {}, pkgs: []};
        (if ($tc.format == "selector" and ($tc.value // "") != "") then $tc.value
         elif (($tc.value // "") != "" and ($tc.path // "") == "") then $tc.value
         else ($tc.path // "") end) as $pkg |
        if ($pkg != "" and (.seen[$pkg] | not)) then
          {seen: (.seen + {($pkg): true}), pkgs: (.pkgs + [$pkg])}
        else . end
      ) | .pkgs
    ' <<< "$req")"

    if [[ "$(jq 'length' <<< "$pkgs_json")" -eq 0 ]]; then
      bktest_err "failed to generate test package list: unable to extract package names from test plan"
    fi

    joined_pkgs="$(jq -r 'join(" ")' <<< "$pkgs_json")"
    cmd_str="$(jq -r 'if .retry then (.config.retry_test_command // .config.test_command) else .config.test_command end' <<< "$req")"
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"

    if [[ "$cmd_str" == *"{{packages}}"* ]]; then
      cmd_str="${cmd_str/\{\{packages\}\}/$joined_pkgs}"
    else
      cmd_str="$cmd_str $joined_pkgs"
    fi
    cmd_str="${cmd_str/\{\{resultPath\}\}/$result_path}"

    if ! words="$(bktest_split_cmd "$cmd_str" 2>&1)"; then
      bktest_err "$words"
    fi

    result_format="$(jq -r '.state.result_format // ""' <<< "$req")"
    if [[ -z "$result_format" ]]; then
      result_format="$(detect_result_format "$(jq -r '.config.test_command // ""' <<< "$req")")"
    fi

    jq -cn --argjson w "$words" --arg rf "$result_format" --arg rp "$result_path" '
      def is_go_test_json:
        (length >= 3) and
        ((.[0] | split("/") | last) == "go") and
        (.[1] == "test") and
        (any(.[]; . == "-json"));
      def jsonfile_arg:
        [range(0; length) as $i |
          if .[$i] == "--jsonfile" and ($i + 1) < length then .[$i+1]
          elif (.[$i] | startswith("--jsonfile=")) then (.[$i] | ltrimstr("--jsonfile="))
          else empty end
        ] | first // "";
      ($w | (is_go_test_json and (jsonfile_arg == ""))) as $captures |
      {
        protocol_version: 1,
        command: $w[0],
        args: $w[1:]
      } + (if ($rf == "go-jsonl" and $captures) then {stdout_tee_path: $rp} else {} end)
    '
    ;;

  interpret-attempt)
    cmd_failed="$(jq -r '.command_failed // false' <<< "$req")"
    signaled="$(jq -r '.signaled // false' <<< "$req")"
    exit_code="$(jq -r '.exit_code // 0' <<< "$req")"
    if [[ "$cmd_failed" == "true" && ( "$signaled" == "true" || "$exit_code" -ne 1 ) ]]; then
      jq -cn '{protocol_version: 1}'
      exit 0
    fi

    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    result_format="$(jq -r '.state.result_format // ""' <<< "$req")"
    if [[ -z "$result_format" ]]; then
      result_format="$(detect_result_format "$(jq -r '.config.test_command // ""' <<< "$req")")"
    fi

    if [[ "$result_format" == "junit" ]]; then
      if ! junit_json="$(bktest_junit_to_json "$result_path" 2>&1)"; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: ${junit_json}" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      jq -cn --argjson tests "$junit_json" '
        reduce $tests[] as $t ({protocol_version: 1, results: []};
          if ($t.name == "TestMain" and $t.classname == "" and $t.has_failure and ($t.failure_content | contains("[build failed]"))) then
            . + {run_error: ("go test failed to build " + $t.suite_name)}
          else
            .results += [{
              test_case: {
                format: "example",
                scope: $t.classname,
                name: $t.name,
                path: $t.classname
              },
              status: $t.result
            }]
          end
        )
      '
    elif [[ "$result_format" == "go-jsonl" ]]; then
      if [[ ! -f "$result_path" ]]; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: failed to read go jsonl: open ${result_path}: no such file or directory" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      if ! parsed="$(jq -cs '
        if length == 0 then error("failed to parse go jsonl: no events found") else
        reduce .[] as $e ({
          pkg_outputs: {},
          pkg_failed: {},
          pkg_order: [],
          pkg_build_failed: {},
          pkg_has_failed_test: {},
          test_statuses: {},
          test_cases: {},
          test_order: []
        };
          ($e.Package // "") as $pkg |
          if $pkg == "" then . else
            (if ($e.Output // "") != "" then .pkg_outputs[$pkg] = ((.pkg_outputs[$pkg] // "") + $e.Output) else . end) |
            if ($e.Test // "") == "" then
              if ($e.Action // "") == "fail" then
                (if (.pkg_failed[$pkg] | not) then .pkg_order += [$pkg] else . end) |
                .pkg_failed[$pkg] = true |
                (if ($e.FailedBuild // "") != "" then .pkg_build_failed[$pkg] = true else . end)
              else . end
            else
              ($e.Action // "") as $act |
              (if $act == "pass" then "passed"
               elif $act == "fail" then "failed"
               elif $act == "skip" then "skipped"
               else "" end) as $st |
              if $st == "" then . else
                ($pkg + "/" + $e.Test) as $k |
                (if (.test_cases[$k] == null) then .test_order += [$k] else . end) |
                (if (.test_statuses[$k] != "failed") then .test_statuses[$k] = $st else . end) |
                (if $st == "failed" then .pkg_has_failed_test[$pkg] = true else . end) |
                .test_cases[$k] = {
                  format: "example",
                  scope: $pkg,
                  name: $e.Test,
                  path: $pkg
                }
              end
            end
          end
        ) |
        . as $acc |
        {
          protocol_version: 1,
          results: [$acc.test_order[] as $k | {test_case: $acc.test_cases[$k], status: $acc.test_statuses[$k]}]
        } |
        reduce $acc.pkg_order[] as $pkg (.;
          if ($acc.pkg_build_failed[$pkg] or (($acc.pkg_outputs[$pkg] // "") | contains("[build failed]"))) then
            . + {run_error: ("go test failed to build " + $pkg)}
          elif ($acc.pkg_has_failed_test[$pkg] | not) then
            .results += [{
              test_case: {
                format: "example",
                scope: $pkg,
                name: "TestMain",
                path: $pkg
              },
              status: "failed"
            }]
          else . end
        )
        end
      ' "$result_path" 2>&1)"; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: ${parsed}" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      printf '%s\n' "$parsed"
    else
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: unsupported Go test result format \"${result_format}\"" \
        '{protocol_version: 1, parse_warning: $warn}'
    fi
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
