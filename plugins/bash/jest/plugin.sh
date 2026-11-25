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
      ($c.test_command // "" | if . == "" then "npx jest --runTestsByPath {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}" else . end) as $test_cmd |
      ($c.test_file_pattern // "" | if . == "" then "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}" else . end) as $pattern |
      ($c.retry_test_command // "" | if . == "" then "npx jest --testNamePattern '\''{{testNamePattern}}'\'' --json --testLocationInResults --outputFile {{resultPath}}" else . end) as $retry_cmd |
      {
        protocol_version: 1,
        runner_id: "jest",
        name: "Jest",
        config: ($c + {
          test_command: $test_cmd,
          retry_test_command: $retry_cmd,
          test_file_pattern: $pattern
        }),
        result_format: "jest-json",
        batch_report_format: "jest-json",
        requires_result_path: true,
        features: {
          split_by_file: true,
          split_by_example: false,
          split_by_selector: true,
          filter_test_files: true,
          filter_test_by_tag: false,
          auto_retry: true,
          mute: true,
          skip: false
        },
        capabilities: {
          discover_targets: true,
          discover_examples: false,
          interpret_attempt: true,
          parse_batch_report: true
        }
      }
    '
    ;;

  discover-targets)
    pattern="$(jq -r '.config.test_file_pattern // "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}"' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    retry="$(jq -r '.retry // false' <<< "$req")"
    if [[ "$retry" == "true" ]]; then
      cmd_str="$(jq -r '.config.retry_test_command // .config.test_command' <<< "$req")"
    else
      cmd_str="$(jq -r '.config.test_command' <<< "$req")"
    fi
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"

    if ! words="$(bktest_split_cmd "$cmd_str" 2>&1)"; then
      bktest_err "$words"
    fi

    if ! out="$(jq -cn --argjson words "$words" --argjson req "$req" --arg rp "$result_path" '
      def quote_meta: gsub("(?<c>[\\\\.+*?()|\\[\\]{}^$])"; "\\" + .c);
      ([($req.test_cases // [])[] | if .format == "selector" then .value else .path end] | sort | unique) as $paths |
      (if ($req.retry // false) then
        ($words | index("{{testNamePattern}}")) as $pidx |
        if $pidx == null then
          error("couldn'\''t find '\''{{testNamePattern}}'\'' sentinel in retry command")
        else
          ([($req.test_cases // [])[] |
            (if (.scope // "") != "" then (.scope + " " + (.name // "")) else (.name // "") end) | quote_meta
          ] | "^(" + join("|") + ")$") as $pat |
          ($words[:$pidx] + [$pat] + $words[$pidx+1:]) |
          (index("{{testExamples}}")) as $tidx |
          if $tidx != null then (.[:$tidx] + $paths + .[$tidx+1:]) else . end
        end
      else
        ($words | index("{{testExamples}}")) as $tidx |
        if $tidx == null then ($words + $paths) else ($words[:$tidx] + $paths + $words[$tidx+1:]) end
      end) as $w1 |
      ($w1 | index("{{resultPath}}")) as $oidx |
      if $oidx == null then
        error("couldn'\''t find '\''{{resultPath}}'\'' sentinel in command, exiting")
      else
        ($w1[:$oidx] + [$rp] + $w1[$oidx+1:]) as $final |
        {protocol_version: 1, command: $final[0], args: $final[1:]}
      end
    ' 2>&1)"; then
      err_msg="$(sed 's/^jq: error (at <unknown>): //' <<< "$out")"
      bktest_err "$err_msg"
    fi
    printf '%s\n' "$out"
    ;;

  interpret-attempt)
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    retry="$(jq -r '.retry // false' <<< "$req")"
    if [[ ! -f "$result_path" ]]; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Jest output, tests will not be retried: failed to read report output: open ${result_path}: no such file or directory" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi
    if ! jq -e . "$result_path" >/dev/null 2>&1; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Jest output, tests will not be retried: failed to parse report output" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi

    wd="$(pwd)"
    # Normalize file names relative to working directory via Python relpath map
    rel_map="$(jq -r '[(.testResults // [])[] | .name // ""] | unique | .[]' "$result_path" | python3 -c '
import json, os, sys
wd = sys.argv[1]
m = {}
for line in sys.stdin:
    p = line.rstrip("\n")
    if p:
        m[p] = os.path.relpath(p, wd).replace(os.sep, "/")
print(json.dumps(m))
' "$wd")"

    jq -c --argjson rel_map "$rel_map" --argjson retry "$retry" '
      def map_status:
        if . == "failed" then "failed"
        elif . == "passed" then "passed"
        elif (. == "pending" or . == "todo") then "skipped"
        else "unknown"
        end;
      reduce (.testResults // [])[] as $tr ({protocol_version: 1, results: [], errored_files: []};
        ($rel_map[$tr.name // ""] // ($tr.name // "")) as $path |
        (if ($tr.status == "failed" and (($tr.assertionResults // []) | length == 0)) then
          .errored_files += [$path]
        else . end) |
        reduce ($tr.assertionResults // [])[] as $ex (.;
          (($ex.status // "") | map_status) as $st |
          if ($retry and $st == "skipped") then .
          else
            .results += [{
              test_case: {
                name: ($ex.title // ""),
                scope: (($ex.ancestorTitles // []) | join(" ")),
                path: $path
              },
              status: $st
            }]
          end
        )
      )
    ' "$result_path"
    ;;

  parse-batch-report)
    fmt="$(jq -r '.format // ""' <<< "$req")"
    if [[ "$fmt" != "jest-json" ]]; then
      bktest_err "unsupported runner report format \"$fmt\""
    fi
    raw="$(jq -r '.report_base64 // ""' <<< "$req" | base64 -d)"
    if ! jq -e '(.testResults | type == "array")' <<< "$raw" >/dev/null 2>&1; then
      bktest_err "incomplete Jest report: missing testResults"
    fi

    wd="$(pwd)"
    rel_map="$(jq -r '[.testResults[] | .name // ""] | unique | .[]' <<< "$raw" | python3 -c '
import json, os, sys
wd = sys.argv[1]
m = {}
for line in sys.stdin:
    p = line.rstrip("\n")
    if not p:
        continue
    if os.path.isabs(p):
        rel = os.path.relpath(p, wd).replace(os.sep, "/")
    else:
        rel = p.replace(os.sep, "/")
    if rel.startswith("./"):
        rel = rel[2:]
    m[p] = rel
print(json.dumps(m))
' "$wd")"

    if ! out="$(jq -c --argjson rel_map "$rel_map" '
      def map_status:
        if . == "passed" then "passed"
        elif . == "failed" then "failed"
        elif (. == "pending" or . == "skipped" or . == "todo" or . == "disabled") then "skipped"
        else error("unknown Jest test status \"" + . + "\"")
        end;
      (.numRuntimeErrorTestSuites // 0) as $num_runtime |
      reduce .testResults[] as $file ({tests: [], seen: {}, errored_files: []};
        ($rel_map[$file.name // ""] // "") as $path |
        if $path == "" then error("jest test file has no name") else
          (reduce ($file.assertionResults // [])[] as $ex ({acc: ., failed: false};
            (($ex.status // "") | map_status) as $st |
            (($ex.ancestorTitles // []) | join(" ")) as $scope |
            ($ex.title // "") as $title |
            ($ex.location.line // 0) as $line |
            (if $line > 0 then ($path + ":" + ($line | tostring)) else "" end) as $loc |
            ($path + "\u0000" + $scope + "\u0000" + $title) as $key |
            (.failed = (.failed or ($st == "failed"))) |
            if (.acc.seen[$key] != null) then
              (.acc.seen[$key]) as $idx |
              (if ($st == "failed" or .acc.tests[$idx].status == "skipped") then
                .acc.tests[$idx].status = $st
              else . end)
            else
              (.acc.tests | length) as $idx |
              .acc.seen[$key] = $idx |
              .acc.tests += [{
                test_case: {scope: $scope, name: $title, path: $path},
                status: $st,
                selector: $path,
                location: $loc
              }]
            end
          )) as $res |
          $res.acc |
          if ($file.status == "failed" and ($res.failed | not)) then
            .errored_files += [$path]
          else . end
        end
      ) |
      {
        protocol_version: 1,
        tests: .tests,
        errored_files: .errored_files,
        errors_outside_tests: ($num_runtime > (.errored_files | length))
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
