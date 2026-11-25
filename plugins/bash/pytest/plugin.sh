#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../_lib/common.sh
source "$SCRIPT_DIR/../_lib/common.sh"

op="${1:-}"
req="$(bktest_read_req)"
bktest_chdir_from_req "$req"

check_python_pkg() {
  local pkg="$1"
  local out
  out="$(python -c 'import importlib.util, sys; print(importlib.util.find_spec(sys.argv[1]) is not None)' "$pkg" 2>/dev/null || true)"
  [[ "$out" == "True" ]]
}

check_collector_version_gt() {
  local required="$1"
  python - "$required" <<'PY'
import importlib.metadata, sys
req = sys.argv[1]
try:
    ver = importlib.metadata.version("buildkite-test-collector")
except Exception as e:
    sys.stderr.write(f"could not determine buildkite-test-collector version: {e}\n")
    sys.exit(2)

def parse(v):
    core = v.split("-", 1)[0].split("+", 1)[0]
    parts = [int(x) for x in core.split(".")]
    while len(parts) < 3:
        parts.append(0)
    return tuple(parts[:3])

try:
    iv = parse(ver)
    rv = parse(req)
except Exception:
    sys.stderr.write(f'could not parse installed buildkite-test-collector version "{ver}"\n')
    sys.exit(2)

if iv <= rv:
    sys.stderr.write(f"buildkite-test-collector version {ver} is installed, but version >{req} is required for --tag-filters support\n")
    sys.exit(2)
PY
}

case "$op" in
  configure)
    persistent="$(jq -r '.config.persistent_runner // false' <<< "$req")"
    tag_filters="$(jq -r '.config.tag_filters // ""' <<< "$req")"
    test_cmd="$(jq -r '.config.test_command // ""' <<< "$req")"
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"

    plugin_installed=false
    if [[ "$persistent" == "true" ]] || check_python_pkg "buildkite_test_collector"; then
      plugin_installed=true
    fi

    use_junit=true
    if [[ "$plugin_installed" == "true" ]]; then
      use_junit=false
      if [[ -n "$tag_filters" ]]; then
        if ! ver_err="$(check_collector_version_gt "1.2.0" 2>&1)"; then
          bktest_err "$ver_err"
        fi
      fi
      if [[ -z "$test_cmd" ]]; then
        test_cmd="pytest {{testExamples}} --json={{resultPath}}"
      fi
      if [[ -z "$result_path" ]]; then
        tmp_dir="$(mktemp -d /tmp/bktec-pytest-XXXXXX)"
        result_path="$tmp_dir/pytest-results.json"
      fi
    else
      if [[ -n "$tag_filters" ]]; then
        bktest_err "--tag-filters requires the 'buildkite-test-collector' package. Please install it with: pip install buildkite-test-collector"
      fi
      if [[ -z "$test_cmd" ]]; then
        test_cmd="pytest {{testExamples}} --junit-xml={{resultPath}}"
      fi
      if [[ -z "$result_path" ]]; then
        tmp_dir="$(mktemp -d /tmp/bktec-pytest-XXXXXX)"
        result_path="$tmp_dir/pytest-results.xml"
      fi
    fi

    if [[ "$test_cmd" == *"--junit-xml"* ]]; then
      use_junit=true
    elif [[ "$test_cmd" == *"--json="* ]]; then
      if [[ "$plugin_installed" != "true" ]]; then
        bktest_err "--json requires the 'buildkite-test-collector' package to be installed. Please install it with: pip install buildkite-test-collector"
      fi
      use_junit=false
    fi

    if [[ "$use_junit" == "true" ]]; then
      warning="Buildkite Test Engine Client: Using JUnit XML output for pytest results."
      result_format="junit"
      batch_format="pytest-junit"
    else
      warning="Buildkite Test Engine Client: Using JSON output for pytest results."
      result_format=""
      batch_format="pytest-json"
    fi

    jq -cn \
      --argjson req "$req" \
      --arg tc "$test_cmd" \
      --arg rp "$result_path" \
      --arg rf "$result_format" \
      --arg bf "$batch_format" \
      --arg warn "$warning" \
      --argjson uj "$use_junit" '
      ($req.config // {}) as $c |
      ($c.test_file_pattern // "" | if . == "" then "**/{*_test,test_*}.py" else . end) as $pattern |
      ($c.retry_test_command // "" | if . == "" then $tc else . end) as $rc |
      {
        protocol_version: 1,
        runner_id: "pytest",
        name: "pytest",
        config: ($c + {
          test_command: $tc,
          retry_test_command: $rc,
          test_file_pattern: $pattern,
          result_path: $rp
        }),
        result_format: $rf,
        batch_report_format: $bf,
        requires_result_path: false,
        pool_non_retryable_exit_codes: true,
        features: {
          split_by_file: true,
          split_by_example: true,
          split_by_selector: true,
          filter_test_files: true,
          filter_test_by_tag: true,
          auto_retry: true,
          mute: true,
          skip: true
        },
        capabilities: {
          discover_targets: true,
          discover_examples: true,
          interpret_attempt: true,
          parse_batch_report: true
        },
        warnings: [$warn],
        state: {
          use_junit: $uj
        }
      }
    '
    ;;

  discover-targets)
    pattern="$(jq -r '.config.test_file_pattern // "**/{*_test,test_*}.py"' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    cmd_str="$(jq -r 'if .retry then (.config.retry_test_command // .config.test_command) else .config.test_command end' <<< "$req")"
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    test_examples="$(jq -c '[(.test_cases // [])[] | if .format == "selector" then .value else .path end]' <<< "$req" | bktest_shellquote_join)"

    if [[ "$cmd_str" == *"{{testExamples}}"* ]]; then
      cmd_str="${cmd_str/\{\{testExamples\}\}/$test_examples}"
    else
      cmd_str="$cmd_str $test_examples"
    fi
    cmd_str="${cmd_str/\{\{resultPath\}\}/$result_path}"

    if ! words="$(bktest_split_cmd "$cmd_str" 2>&1)"; then
      bktest_err "$words"
    fi
    jq -cn --argjson w "$words" '{protocol_version: 1, command: $w[0], args: $w[1:]}'
    ;;

  discover-examples)
    file_count="$(jq '(.files // []) | length' <<< "$req")"
    if [[ "$file_count" -eq 0 ]]; then
      jq -cn '{protocol_version: 1, examples: []}'
      exit 0
    fi

    tag_filters="$(jq -r '.config.tag_filters // ""' <<< "$req")"
    use_junit="$(jq -r '.state.use_junit // false' <<< "$req")"
    mapfile -t files_arr < <(jq -r '.files[]' <<< "$req")

    args=(--collect-only -q)
    if [[ -n "$tag_filters" ]]; then
      args+=(--tag-filters "$tag_filters")
    fi
    args+=("${files_arr[@]}")

    stderr_file="$(mktemp /tmp/pytest-collect-stderr-XXXXXX)"
    trap 'rm -f "$stderr_file"' EXIT
    if ! out="$(pytest "${args[@]}" 2>"$stderr_file")"; then
      err_txt="$(cat "$stderr_file")"
      bktest_err "pytest collection failed: $err_txt"
    fi

    printf '%s\n' "$out" | jq -Rsc --argjson uj "$use_junit" '
      def collector_to_junit:
        (index("::")) as $ci |
        (if $ci != null then
          ((.[:$ci] | sub("\\.py$"; "")) + .[$ci:])
        else
          sub("\\.py$"; "")
        end) | gsub("::"; ".") | gsub("/"; ".");
      {
        protocol_version: 1,
        examples: [
          split("\n")[] |
          gsub("^\\s+|\\s+$"; "") |
          select(. != "" and contains("::")) |
          . as $nid |
          (split("::")) as $parts |
          ($parts[:-1] | join("::")) as $raw_scope |
          ($parts[-1]) as $name |
          {
            identifier: $nid,
            path: $nid,
            scope: (if $uj then ($raw_scope | collector_to_junit) else $raw_scope end),
            name: $name,
            format: "example"
          }
        ]
      }
    '
    ;;

  interpret-attempt)
    use_junit="$(jq -r '.state.use_junit // false' <<< "$req")"
    cmd_failed="$(jq -r '.command_failed // false' <<< "$req")"
    signaled="$(jq -r '.signaled // false' <<< "$req")"
    exit_code="$(jq -r '.exit_code // 0' <<< "$req")"
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"

    parse_exit2_json=false
    if [[ "$cmd_failed" == "true" && ( "$signaled" == "true" || "$exit_code" -ne 1 ) ]]; then
      if [[ "$use_junit" == "true" || "$signaled" == "true" || "$exit_code" -ne 2 ]]; then
        jq -cn '{protocol_version: 1}'
        exit 0
      fi
      parse_exit2_json=true
    fi

    if [[ "$use_junit" == "true" ]]; then
      if ! junit_json="$(bktest_junit_to_json "$result_path" 2>&1)"; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: ${junit_json}" \
          '{protocol_version: 1, parse_warning: $warn}'
        exit 0
      fi
      jq -cn --argjson tests "$junit_json" '
        def junit_node_id($cls; $name):
          if $cls == "" then $name else
            ($cls | split(".")) as $parts |
            ([range(0; ($parts | length)) | select(($parts[.] | startswith("test_")) or ($parts[.] | endswith("_test")))]) as $mod_matches |
            (if ($mod_matches | length) > 0 then (($mod_matches | last) + 1) else
              ([range(0; ($parts | length)) | select(($parts[.] | test("^[A-Z]")))] | first // ($parts | length))
            end) as $mod_end |
            (($parts[:$mod_end] | join("/")) + ".py") as $mod_path |
            ($parts[$mod_end:]) as $cls_parts |
            if ($cls_parts | length) == 0 then ($mod_path + "::" + $name)
            else ($mod_path + "::" + ($cls_parts | join("::")) + "::" + $name) end
          end;
        {
          protocol_version: 1,
          results: [
            $tests[] |
            junit_node_id(.classname; .name) as $path |
            {
              test_case: {
                identifier: $path,
                format: "example",
                scope: .classname,
                name: .name,
                path: $path
              },
              status: .result
            }
          ]
        }
      '
    else
      if [[ ! -f "$result_path" ]]; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: failed to read json: open ${result_path}: no such file or directory" \
          --argjson p2 "$parse_exit2_json" \
          '{protocol_version: 1, parse_warning: $warn} + (if $p2 then {use_command_error_as_run_error: true} else {} end)'
        exit 0
      fi
      if ! parsed="$(jq -c --argjson p2 "$parse_exit2_json" '
        reduce .[] as $t ({protocol_version: 1, results: []};
          ($t.scope // "") as $scope |
          ($t.name // "") as $name |
          (if $scope == "" then $name else ($scope + "::" + $name) end) as $path |
          .results += [{
            test_case: {
              identifier: ($t.id // ""),
              format: "example",
              scope: $scope,
              name: $name,
              path: $path
            },
            status: ($t.result // "unknown")
          }] |
          if (($t.tags["test.pytest_collection_error"] // "") == "true") then
            . + {run_error: ("pytest collection failed: " + $path)}
          else . end
        ) |
        if ($p2 and ((.run_error // "") == "")) then
          . + {use_command_error_as_run_error: true}
        else . end
      ' "$result_path" 2>&1)"; then
        jq -cn --arg warn "Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: failed to parse json: ${parsed}" \
          --argjson p2 "$parse_exit2_json" \
          '{protocol_version: 1, parse_warning: $warn} + (if $p2 then {use_command_error_as_run_error: true} else {} end)'
        exit 0
      fi
      printf '%s\n' "$parsed"
    fi
    ;;

  parse-batch-report)
    fmt="$(jq -r '.format // ""' <<< "$req")"
    raw="$(jq -r '.report_base64 // ""' <<< "$req" | base64 -d)"

    if [[ "$fmt" == "pytest-json" ]]; then
      if ! jq -e 'type == "array"' <<< "$raw" >/dev/null 2>&1; then
        bktest_err "invalid pytest JSON report: not a list of tests"
      fi
      if ! out="$(jq -c '
        reduce .[] as $t ({tests: [], seen: {}, errored_files: []};
          ($t.scope // "") as $scope |
          ($t.name // "") as $name |
          (if $scope == "" then $name else ($scope + "::" + $name) end) as $path |
          if (($t.tags["test.pytest_collection_error"] // "") == "true") then
            ($path | split("::") | first) as $file |
            if (.errored_files | index($file)) == null then .errored_files += [$file] else . end
          else
            ($t.result // "") as $st |
            if ($st != "passed" and $st != "failed" and $st != "skipped") then
              error("unknown pytest test status \"" + $st + "\"")
            else
              ($path | split("::") | first) as $file |
              if ($file == "" or $name == "") then
                error("pytest test \"" + $path + "\" has no file or name")
              elif (.seen[$path] != null) then
                (.seen[$path]) as $idx |
                if ($st == "failed" or .tests[$idx].status == "skipped") then
                  .tests[$idx].status = $st
                else . end
              else
                (.tests | length) as $idx |
                .seen[$path] = $idx |
                .tests += [{
                  test_case: {identifier: $path, scope: $scope, name: $name, path: $path},
                  status: $st,
                  selector: $file
                }]
              end
            end
          end
        ) |
        {protocol_version: 1, tests: .tests, errored_files: .errored_files}
      ' <<< "$raw" 2>&1)"; then
        err_msg="$(sed 's/^jq: error (at <stdin>:[0-9]*): //' <<< "$out")"
        bktest_err "$err_msg"
      fi
      printf '%s\n' "$out"
    elif [[ "$fmt" == "pytest-junit" ]]; then
      if ! junit_json="$(printf '%s' "$raw" | bktest_junit_to_json "-" 2>&1)"; then
        bktest_err "invalid pytest JUnit XML report: $junit_json"
      fi
      if ! out="$(jq -cn --argjson tests "$junit_json" '
        def junit_node_id($cls; $name):
          if $cls == "" then $name else
            ($cls | split(".")) as $parts |
            ([range(0; ($parts | length)) | select(($parts[.] | startswith("test_")) or ($parts[.] | endswith("_test")))]) as $mod_matches |
            (if ($mod_matches | length) > 0 then (($mod_matches | last) + 1) else
              ([range(0; ($parts | length)) | select(($parts[.] | test("^[A-Z]")))] | first // ($parts | length))
            end) as $mod_end |
            (($parts[:$mod_end] | join("/")) + ".py") as $mod_path |
            ($parts[$mod_end:]) as $cls_parts |
            if ($cls_parts | length) == 0 then ($mod_path + "::" + $name)
            else ($mod_path + "::" + ($cls_parts | join("::")) + "::" + $name) end
          end;
        reduce $tests[] as $t ({tests: [], seen: {}, errors_outside_tests: false};
          if $t.classname == "" then
            if $t.result != "skipped" then .errors_outside_tests = true else . end
          else
            junit_node_id($t.classname; $t.name) as $path |
            ($path | split("::") | first) as $file |
            ($t.result) as $st |
            if ($file == "" or $t.name == "") then
              error("pytest test \"" + $path + "\" has no file or name")
            elif (.seen[$path] != null) then
              (.seen[$path]) as $idx |
              if ($st == "failed" or .tests[$idx].status == "skipped") then
                .tests[$idx].status = $st
              else . end
            else
              (.tests | length) as $idx |
              .seen[$path] = $idx |
              .tests += [{
                test_case: {identifier: $path, scope: $t.classname, name: $t.name, path: $path},
                status: $st,
                selector: $file
              }]
            end
          end
        ) |
        {protocol_version: 1, tests: .tests, errors_outside_tests: .errors_outside_tests}
      ' 2>&1)"; then
        err_msg="$(sed 's/^jq: error (at <unknown>): //' <<< "$out")"
        bktest_err "$err_msg"
      fi
      printf '%s\n' "$out"
    else
      bktest_err "unsupported runner report format \"$fmt\""
    fi
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
