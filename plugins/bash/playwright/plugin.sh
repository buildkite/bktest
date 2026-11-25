#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../_lib/common.sh
source "$SCRIPT_DIR/../_lib/common.sh"

op="${1:-}"
req="$(bktest_read_req)"
bktest_chdir_from_req "$req"

build_pw_words() {
  local req_json="$1"
  local retry_flag="$2"
  local paths_json="$3"
  local cmd_str words

  if [[ "$retry_flag" == "true" ]]; then
    cmd_str="$(jq -r '.config.retry_test_command // .config.test_command' <<< "$req_json")"
  else
    cmd_str="$(jq -r '.config.test_command' <<< "$req_json")"
  fi

  if ! words="$(bktest_split_cmd "$cmd_str" 2>&1)"; then
    bktest_err "$words"
  fi

  jq -cn --argjson words "$words" --argjson paths "$paths_json" '
    ($words | index("{{testExamples}}")) as $ti |
    if $ti == null then ($words + $paths) else ($words[:$ti] + $paths + $words[$ti+1:]) end
  '
}

case "$op" in
  configure)
    jq -cn --argjson req "$req" '
      ($req.config // {}) as $c |
      ($c.test_command // "" | if . == "" then "npx playwright test" else . end) as $test_cmd |
      ($c.retry_test_command // "" | if . == "" then $test_cmd else . end) as $retry_cmd |
      ($c.test_file_pattern // "" | if . == "" then "**/{*.spec,*.test}.{ts,js}" else . end) as $pattern |
      ($c.result_path // "") as $rp |
      {
        protocol_version: 1,
        runner_id: "playwright",
        name: "Playwright",
        config: ($c + {
          test_command: $test_cmd,
          retry_test_command: $retry_cmd,
          test_file_pattern: $pattern
        }),
        result_format: (if $rp != "" then "playwright-json" else "" end),
        batch_report_format: "playwright-json",
        requires_result_path: true,
        features: {
          split_by_file: true,
          split_by_example: true,
          split_by_selector: true,
          filter_test_files: true,
          filter_test_by_tag: false,
          auto_retry: true,
          mute: true,
          skip: false
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
    pattern="$(jq -r '.config.test_file_pattern // "**/{*.spec,*.test}.{ts,js}"' <<< "$req")"
    exclude="$(jq -r '.config.test_file_exclude_pattern // ""' <<< "$req")"
    if ! targets="$(bktest_discover_files "$pattern" "$exclude" 2>&1)"; then
      bktest_err "$targets"
    fi
    jq -cn --argjson targets "$targets" '{protocol_version: 1, targets: $targets}'
    ;;

  prepare-attempt)
    retry="$(jq -r '.retry // false' <<< "$req")"
    paths_json="$(jq -c '[(.test_cases // [])[] | if .format == "selector" then .value else .path end]' <<< "$req")"
    if ! final_words="$(build_pw_words "$req" "$retry" "$paths_json")"; then
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

    tmp_file="$(mktemp /tmp/playwright-list-XXXXXX.json)"
    stderr_file="$(mktemp /tmp/playwright-list-stderr-XXXXXX)"
    trap 'rm -f "$tmp_file" "$stderr_file"' EXIT

    files_json="$(jq -c '.files // []' <<< "$req")"
    if ! final_words="$(build_pw_words "$req" "false" "$files_json")"; then
      printf '%s\n' "$final_words"
      exit 1
    fi
    mapfile -t cmd_arr < <(jq -r '.[]' <<< "$final_words")
    cmd_arr+=(--list --reporter=json)

    if ! PLAYWRIGHT_JSON_OUTPUT_NAME="$tmp_file" "${cmd_arr[@]}" >/dev/null 2>"$stderr_file"; then
      err_txt="$(cat "$stderr_file")"
      bktest_err "failed to list playwright tests: stderr: $err_txt"
    fi

    if ! jq -e . "$tmp_file" >/dev/null 2>&1; then
      bktest_err "failed to parse playwright test list output"
    fi

    jq -c '
      def walk_suite($name):
        [
          (.specs // [])[] |
          ((.tests // [])[0].projectName // "") as $proj |
          {
            name: (.title // ""),
            path: ((.file // "") + ":" + ((.line // 0) | tostring)),
            scope: (" " + $proj + " " + $name + " " + (.title // ""))
          }
        ] + [
          (.suites // [])[] | walk_suite($name + " " + (.title // ""))[]
        ];
      {
        protocol_version: 1,
        examples: [(.suites // [])[] | walk_suite(.title // "")[]]
      }
    ' "$tmp_file"
    ;;

  interpret-attempt)
    result_path="$(jq -r '.config.result_path // ""' <<< "$req")"
    if [[ ! -f "$result_path" ]]; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: failed to read playwright output: open ${result_path}: no such file or directory" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi
    if ! jq -e . "$result_path" >/dev/null 2>&1; then
      jq -cn --arg warn "Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: failed to parse playwright output" \
        '{protocol_version: 1, parse_warning: $warn}'
      exit 0
    fi

    wd="$(pwd)"
    # Compute relative error file locations via python relpath
    err_files_info="$(jq -c '.errors // []' "$result_path" | python3 -c '
import json, os, sys
wd = sys.argv[1]
errs = json.load(sys.stdin)
files = []
ok = True
for e in errs:
    loc = e.get("location") if isinstance(e, dict) else None
    f = loc.get("file") if isinstance(loc, dict) else ""
    if not f:
        ok = False
        break
    rel = os.path.relpath(f, wd).replace(os.sep, "/")
    if rel not in files:
        files.append(rel)
print(json.dumps({"ok": ok, "files": files}))
' "$wd")"

    jq -c --argjson req "$req" --argjson err_info "$err_files_info" '
      def walk_results($name):
        [
          (.specs // [])[] |
          ((.tests // [])[0].projectName // "") as $proj |
          (if (.ok | not) then "failed"
           elif (((.tests // [])[0].status // "") == "skipped") then "skipped"
           else "passed" end) as $base_status |
          (if .ok and any((.tests // [])[]; .status == "skipped" and .expectedStatus != "skipped") then
            "not_run"
           else $base_status end) as $final_status |
          {
            test_case: {
              name: (.title // ""),
              path: ((.file // "") + ":" + ((.line // 0) | tostring)),
              scope: (" " + $proj + " " + $name + " " + (.title // ""))
            },
            status: $final_status
          }
        ] + [
          (.suites // [])[] | walk_results($name + " " + (.title // ""))[]
        ];
      ([(.suites // [])[] | walk_results(.title // "")[]]) as $results |
      ((.errors // []) | length) as $err_count |
      if $err_count == 0 then
        {protocol_version: 1, results: $results}
      else
        ($err_info.files) as $err_files |
        (reduce ($req.test_cases // [])[] as $tc ({unmatched: $err_files, remaining: []};
          (if $tc.format == "selector" then ($tc.value // "") else ($tc.path // "") end) as $raw_file |
          ($raw_file | sub(":[0-9]+$"; "") | ltrimstr("./")) as $file |
          if ($err_files | index($file)) != null then
            .unmatched |= map(select(. != $file))
          else
            .remaining += [$tc]
          end
        )) as $match |
        if (($err_info.ok | not) or ((.suites // []) | length > 0) or (($match.unmatched | length) > 0)) then
          {protocol_version: 1, results: $results, run_error: "Playwright failed with errors"}
        else
          {
            protocol_version: 1,
            results: $results,
            errored_files: $err_files
          } + (
            if ($match.remaining | length) > 0 then
              {
                rerun_test_cases: $match.remaining,
                rerun_message: ("+++ Buildkite Test Engine Client: Playwright failed to load " + ($err_files | join(", ")) + ". Running the other test files again.")
              }
            else {} end
          )
        end
      end
    ' "$result_path"
    ;;

  parse-batch-report)
    fmt="$(jq -r '.format // ""' <<< "$req")"
    if [[ "$fmt" != "playwright-json" ]]; then
      bktest_err "unsupported runner report format \"$fmt\""
    fi
    raw="$(jq -r '.report_base64 // ""' <<< "$req" | base64 -d)"
    if ! jq -e '(.suites | type == "array") and ((.config.rootDir // "") != "")' <<< "$raw" >/dev/null 2>&1; then
      bktest_err "incomplete Playwright report: missing suites or config.rootDir"
    fi

    wd="$(pwd)"
    if ! out="$(python3 -c '
import json, os, sys

wd = sys.argv[1]
report = json.loads(sys.stdin.read())
root_dir = report["config"]["rootDir"]

tests = []
seen = {}
errored_files = []
errors_outside = False
errored_stopped = False

def walk(suite, titles):
    for spec in suite.get("specs") or []:
        f = spec.get("file") or ""
        title = spec.get("title") or ""
        line = spec.get("line") or 0
        if not f:
            raise ValueError(f"playwright test \"{title}\" has no file")
        rel = os.path.relpath(os.path.join(root_dir, f), wd).replace(os.sep, "/")
        for run in spec.get("tests") or []:
            rs = run.get("status") or ""
            if rs in ("expected", "flaky"):
                st = "passed"
            elif rs == "unexpected":
                st = "failed"
            elif rs == "skipped":
                st = "skipped"
                if run.get("expectedStatus") != "skipped" and rel not in errored_files:
                    errored_files.append(rel)
            else:
                raise ValueError(f"unknown Playwright test status \"{rs}\"")
            proj = run.get("projectName") or ""
            scope = f" {proj} {titles} {title}"
            tc = {
                "test_case": {
                    "scope": scope,
                    "name": title,
                    "path": f"{rel}:{line}",
                },
                "status": st,
                "selector": rel,
            }
            key = rel + "\x00" + scope
            if key in seen:
                idx = seen[key]
                if st == "failed" or tests[idx]["status"] == "skipped":
                    tests[idx]["status"] = st
                continue
            seen[key] = len(tests)
            tests.append(tc)
    for child in suite.get("suites") or []:
        walk(child, titles + " " + (child.get("title") or ""))

try:
    for s in report["suites"]:
        walk(s, s.get("title") or "")
except ValueError as e:
    sys.stderr.write(str(e) + "\n")
    sys.exit(2)

errs = report.get("errors") or []
if errs:
    files = []
    ok = True
    for e in errs:
        loc = e.get("location") if isinstance(e, dict) else None
        ef = loc.get("file") if isinstance(loc, dict) else ""
        if not ef:
            ok = False
            break
        rel = os.path.relpath(ef, wd).replace(os.sep, "/")
        if rel not in files:
            files.append(rel)
    if ok and len(tests) == 0:
        errored_files = files
        errored_stopped = True
    else:
        errors_outside = True

print(json.dumps({
    "protocol_version": 1,
    "tests": tests,
    "errors_outside_tests": errors_outside,
    "errored_files": errored_files,
    "errored_files_stopped_run": errored_stopped,
    "unselected_tests": True,
}))
' "$wd" <<< "$raw" 2>&1)"; then
      bktest_err "$out"
    fi
    printf '%s\n' "$out"
    ;;

  *)
    bktest_err "unknown plugin operation \"$op\""
    ;;
esac
