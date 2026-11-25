#!/usr/bin/env bash
# Bash runner plugin for go test. JUnit XML reports are parsed with python3;
# go test -json and gotestsum --jsonfile reports only need jq.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# gotest_is_json_command reports whether the arguments run `go test -json`.
gotest_is_json_command() {
  (($# >= 3)) && [[ ${1##*/} == go && $2 == test ]] || return 1
  local arg
  for arg in "$@"; do [[ $arg == -json ]] && return 0; done
  return 1
}

# gotest_jsonfile_arg prints the value of gotestsum's --jsonfile argument.
gotest_jsonfile_arg() {
  while (($#)); do
    case $1 in
      --jsonfile) if (($# > 1)); then printf '%s' "$2"; return; fi ;;
      --jsonfile=*) printf '%s' "${1#--jsonfile=}"; return ;;
    esac
    shift
  done
}

plugin_initialize() {
  local command format=junit
  local -a words
  command=$(bk_param '.config.test_command // "gotestsum --jsonfile={{resultPath}} {{packages}}"')
  if mapfile -d '' words < <(bk_split_words "$command" 2>/dev/null) && ((${#words[@]})); then
    if gotest_is_json_command "${words[@]}" || [[ -n $(gotest_jsonfile_arg "${words[@]}") ]]; then format=go-jsonl; fi
  elif [[ $command == *--jsonfile* || $command == *-json* ]]; then
    format=go-jsonl
  fi
  jq --arg command "$command" --arg format "$format" '.config
    | .test_command = $command
    | .retry_test_command = (.retry_test_command // .test_command)
    | .format = $format' <<<"$BK_PARAMS" | bk_save_config
  jq '{
    protocol_version: 1,
    name: "gotest",
    features: {split_by_selector: true, auto_retry: true, mute: true},
    result_format: .format,
    result_path: (.result_path // ""),
    location_prefix: (.location_prefix // "")
  }' "$BK_STATE_DIR/config.json"
}

# Discover lists the packages that have test files: packages are the
# smallest unit go test can split on.
plugin_discover() {
  local output
  bk_log 'Discovering Go packages with tests using `go list`'
  if ! output=$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... 2>"$BK_STATE_DIR/stderr"); then
    bk_fail "go list failed: $(cat "$BK_STATE_DIR/stderr")"
  fi
  output=$(grep -v '^$' <<<"$output")
  if [[ -z $output ]]; then
    bk_fail 'no Go packages with tests found using `go list`'
  fi
  jq -Rn '{targets: [inputs]}' <<<"$output"
}

plugin_examples() {
  bk_fail "go test does not support split by example"
}

plugin_command() {
  local cmd packages result_path
  local -a words
  packages=$(jq -r '[.test_cases[]
    | if ((.format == "selector" and (.value // "") != "") or ((.value // "") != "" and (.path // "") == "")) then .value else .path end
    | select(. != "" and . != null)] | reduce .[] as $p ([]; if index([$p]) then . else . + [$p] end) | join(" ")' <<<"$BK_PARAMS")
  if [[ -z $packages ]]; then
    bk_fail "failed to generate test package list: unable to extract package names from test plan"
  fi
  bk_log "Packages: $packages"
  if [[ $(bk_param .retry) == true ]]; then cmd=$(bk_cfg retry_test_command); else cmd=$(bk_cfg test_command); fi
  if [[ $cmd == *'{{packages}}'* ]]; then
    cmd=${cmd/'{{packages}}'/$packages}
  else
    cmd="$cmd $packages"
  fi
  result_path=$(bk_cfg result_path)
  cmd=${cmd/'{{resultPath}}'/$result_path}
  bk_words words "$cmd"
  # `go test -json` writes its report to stdout, so bktest saves a copy.
  if [[ $(bk_cfg format) == go-jsonl ]] && gotest_is_json_command "${words[@]}" && [[ -z $(gotest_jsonfile_arg "${words[@]}") ]]; then
    bk_json_array "${words[@]}" | jq -c --arg path "$result_path" '{argv: ., stdout_path: $path}'
  else
    bk_command_result "${words[@]}"
  fi
}

# A package that fails to build errors the run, which retrying can't fix. A
# package that failed without a failed test, such as a crashing TestMain, is
# reported as its TestMain so it can be retried.
# shellcheck disable=SC2016
GO_JSONL_PROGRAM='
  [.[] | select((.Package // "") != "")] as $events
  | if length == 0 then error("failed to parse go jsonl: no events found") else . end
  | ($events | reduce .[] as $e ({order: [], tests: {}, pkgs: [], failed: {}, build: {}, output: {}, has_failed: {}};
      .output[$e.Package] += ($e.Output // "")
      | if ($e.Test // "") == "" then
          if $e.Action == "fail" then
            (if .failed[$e.Package] then . else .pkgs += [$e.Package] end)
            | .failed[$e.Package] = true
            | if ($e.FailedBuild // "") != "" then .build[$e.Package] = true else . end
          else . end
        else
          ({pass: "passed", fail: "failed", skip: "skipped"}[$e.Action]) as $status
          | if $status == null then . else
              ($e.Package + "/" + $e.Test) as $key
              | (if .tests[$key] then . else .order += [$key] end)
              | .tests[$key] = {test_case: {format: "example", scope: $e.Package, name: $e.Test, path: $e.Package},
                  status: (if .tests[$key].status == "failed" then "failed" else $status end)}
              | if $status == "failed" then .has_failed[$e.Package] = true else . end
            end
        end)) as $s
  | {tests: ([$s.order[] as $k | $s.tests[$k]]
      + [$s.pkgs[] | select(($s.build[.] or ($s.output[.] | contains("[build failed]")) or $s.has_failed[.]) | not)
         | {test_case: {format: "example", scope: ., name: "TestMain", path: .}, status: "failed"}])}
  + ([$s.pkgs[] | select($s.build[.] or ($s.output[.] | contains("[build failed]")))] | last
     | if . then {error: "go test failed to build \(.)"} else {} end)
'

plugin_interpret() {
  local code report
  code=$(bk_param '.exit_code // 0')
  # go test exits 1 for both build and test failures; other codes leave no
  # report worth reading.
  if [[ $code != 0 && $code != 1 ]]; then
    echo '{}'
    return
  fi
  report=$(bk_cfg result_path)
  if [[ $(bk_cfg format) == go-jsonl ]]; then
    jq -s "$GO_JSONL_PROGRAM" "$report" 2>/dev/null && return
  else
    # gotestsum reports a package that fails to build as a "TestMain" with no
    # classname and a "[build failed]" failure.
    # shellcheck disable=SC2016
    bk_junit_json "$report" 2>/dev/null | jq '
      (map(select(.name == "TestMain" and .classname == "" and ((.failure // "") | contains("[build failed]"))))) as $build
      | {tests: [.[] | select(.name == "TestMain" and .classname == "" and ((.failure // "") | contains("[build failed]")) | not)
          | {test_case: {format: "example", scope: .classname, name: .name, path: .classname}, status: .result}]}
      + ($build | last | if . then {error: "go test failed to build \(.suite)"} else {} end)' 2>/dev/null && return
  fi
  jq -n --arg path "$report" '{messages: ["Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: failed to read \($path)"]}'
}

bk_serve
