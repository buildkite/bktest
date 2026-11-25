#!/usr/bin/env bash
# Bash runner plugin for pytest. JUnit XML reports are parsed with python3.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# pytest_collector_installed reports whether the Buildkite test collector's
# Python package is importable.
pytest_collector_installed() {
  [[ "$(python -c 'import importlib.util, sys; print(importlib.util.find_spec(sys.argv[1]) is not None)' buildkite_test_collector 2>/dev/null)" == True ]]
}

# pytest_check_collector_version requires a collector newer than 1.2.0 for
# --tag-filters.
pytest_check_collector_version() {
  local installed
  if ! installed=$(python -c 'import importlib.metadata, sys; print(importlib.metadata.version(sys.argv[1]))' buildkite-test-collector 2>/dev/null); then
    echo "could not determine buildkite-test-collector version"
    return 1
  fi
  if [[ "$(printf '%s\n%s\n' "$installed" 1.2.0 | sort -V | tail -1)" == 1.2.0 ]]; then
    echo "buildkite-test-collector version $installed is installed, but version >1.2.0 is required for --tag-filters support"
    return 1
  fi
}

# Initialize uses the collector's JSON when its package is installed, and
# JUnit XML otherwise. A --junit-xml or --json= flag in the command wins.
plugin_initialize() {
  local collector=false use_junit command result_path err tag_filters
  pytest_collector_installed && collector=true
  use_junit=$([[ $collector == true ]] && echo false || echo true)
  tag_filters=$(bk_param '.config.tag_filters // ""')
  command=$(bk_param '.config.test_command // ""')
  result_path=$(bk_param '.config.result_path // ""')

  if [[ $collector == true ]]; then
    if [[ -n $tag_filters ]] && ! err=$(pytest_check_collector_version); then
      printf 'Error: %s\nPlease upgrade with: pip install --upgrade buildkite-test-collector\n' "$err" >&2
      bk_fail "$err"
    fi
    [[ -n $command ]] || command='pytest {{testExamples}} --json={{resultPath}}'
    [[ -n $result_path ]] || result_path="$(mktemp -d "${TMPDIR:-/tmp}/bktec-pytest-XXXXXX")/pytest-results.json"
  else
    if [[ -n $tag_filters ]]; then
      printf "Error: --tag-filters requires the 'buildkite-test-collector' package.\nPlease install it with: pip install buildkite-test-collector.\n" >&2
      bk_fail "--tag-filters requires the 'buildkite-test-collector' package"
    fi
    [[ -n $command ]] || command='pytest {{testExamples}} --junit-xml={{resultPath}}'
    [[ -n $result_path ]] || result_path="$(mktemp -d "${TMPDIR:-/tmp}/bktec-pytest-XXXXXX")/pytest-results.xml"
  fi

  if [[ $command == *--junit-xml* ]]; then
    use_junit=true
  elif [[ $command == *--json=* ]]; then
    if [[ $collector != true ]]; then
      printf "Error: --json requires the 'buildkite-test-collector' package to be installed.\nPlease install it with: pip install buildkite-test-collector.\n" >&2
      bk_fail "--json requires the 'buildkite-test-collector' package to be installed"
    fi
    use_junit=false
  fi
  if [[ $use_junit == true ]]; then
    echo "Buildkite Test Engine Client: Using JUnit XML output for pytest results." >&2
  else
    echo "Buildkite Test Engine Client: Using JSON output for pytest results." >&2
  fi

  jq --arg command "$command" --arg result_path "$result_path" --argjson use_junit "$use_junit" '.config
    | .test_command = $command
    | .result_path = $result_path
    | .use_junit = $use_junit
    | .test_file_pattern = (.test_file_pattern // "**/{*_test,test_*}.py")
    | .retry_test_command = (.retry_test_command // .test_command)' <<<"$BK_PARAMS" | bk_save_config
  # The collector uploads its own JSON, so bktest only uploads JUnit.
  jq '{
    protocol_version: 1,
    name: "pytest",
    features: {split_by_file: true, split_by_example: true, filter_test_files: true, filter_test_by_tag: true,
      auto_retry: true, mute: true, skip: true, split_by_selector: true},
    result_format: (if .use_junit then "junit" else "" end),
    result_path: .result_path,
    location_prefix: (.location_prefix // "")
  }' "$BK_STATE_DIR/config.json"
}

plugin_discover() {
  bk_discover_files
}

# Examples lists node IDs with --collect-only, filtered by execution tags.
# JUnit scopes are dotted classnames rather than the collector's file::class.
plugin_examples() {
  local output tag_filters
  local -a args files
  mapfile -t files < <(bk_param '.files[]')
  if ((${#files[@]} == 0)); then
    echo '{"test_cases": []}'
    return
  fi
  args=(--collect-only -q)
  tag_filters=$(bk_cfg tag_filters)
  [[ -z $tag_filters ]] || args+=(--tag-filters "$tag_filters")
  if ! output=$(pytest "${args[@]}" "${files[@]}" 2>"$BK_STATE_DIR/stderr" </dev/null); then
    bk_fail "pytest collection failed: $(cat "$BK_STATE_DIR/stderr")"
  fi
  # shellcheck disable=SC2016
  printf '%s\n' "$output" | jq -Rn --argjson use_junit "$(bk_cfg use_junit)" '{test_cases: [inputs
    | gsub("^\\s+|\\s+$"; "") | select(contains("::"))
    | . as $id | (rindex("::")) as $i | {scope: $id[:$i], name: $id[$i + 2:]}
    | if $use_junit then .scope |= (
        (index("::")) as $j
        | (if $j then [.[:$j], .[$j + 2:]] else [., null] end) as [$path, $rest]
        | ($path | if endswith(".py") then .[:-3] else . end) + (if $rest then "::" + $rest else "" end)
        | gsub("::"; ".") | gsub("/"; "."))
      else . end
    | {identifier: $id, path: $id, scope, name, format: "example"}]}'
}

plugin_command() {
  local cmd examples
  local -a paths
  if [[ $(bk_param .retry) == true ]]; then cmd=$(bk_cfg retry_test_command); else cmd=$(bk_cfg test_command); fi
  mapfile -t paths < <(bk_paths)
  examples=$(bk_shell_join "${paths[@]}")
  if [[ $cmd == *'{{testExamples}}'* ]]; then
    cmd=${cmd/'{{testExamples}}'/$examples}
  else
    cmd="$cmd $examples"
  fi
  cmd=${cmd/'{{resultPath}}'/$(bk_cfg result_path)}
  local -a words
  bk_words words "$cmd"
  bk_command_result "${words[@]}"
}

# pytest exits 1 for failed tests. For exit code 2, collection errors, the
# collector still reports tagged results, but the run errors.
plugin_interpret() {
  local code report use_junit tests
  code=$(bk_param '.exit_code // 0')
  report=$(bk_cfg result_path)
  use_junit=$(bk_cfg use_junit)
  if [[ $code != 0 && $code != 1 ]] && [[ $use_junit == true || $code != 2 ]]; then
    echo '{}'
    return
  fi

  if [[ $use_junit == true ]]; then
    # The JUnit scope is the raw classname. The node ID's module is the last
    # component named like a test file, or else everything before the first
    # CamelCase component.
    # shellcheck disable=SC2016
    tests=$(bk_junit_json "$report" 2>/dev/null | jq -c '[.[] | (
      if .classname == "" then .name else
        (.classname | split(".")) as $parts
        | ([range(0; $parts | length) | select($parts[.] | startswith("test_") or endswith("_test")) | . + 1] | last) as $end
        | ($end // ([range(0; $parts | length) | select($parts[.] | test("^[A-Z]"))] | first) // ($parts | length)) as $end
        | ($parts[:$end] | join("/")) + ".py" + ($parts[$end:] | map("::" + .) | join("")) + "::" + .name
      end) as $path
      | {test_case: {identifier: $path, format: "example", scope: .classname, name: .name, path: $path}, status: .result}]')
  else
    # shellcheck disable=SC2016
    tests=$(jq -c '[.[] | (if (.scope // "") == "" then .name else .scope + "::" + .name end) as $path
      | {test_case: {identifier: .id, format: "example", scope: .scope, name: .name, path: $path}, status: .result,
         collection_error: (.tags["test.pytest_collection_error"] == "true")}]' "$report" 2>/dev/null)
  fi
  printf '%s' "${tests:-null}" >"$BK_STATE_DIR/tests.json"
  # shellcheck disable=SC2016
  jq -n --slurpfile tests "$BK_STATE_DIR/tests.json" --argjson code "$code" --arg error "$(bk_param '.error // ""')" --arg report "$report" '
    $tests[0] as $tests
    | if $tests == null then {messages: ["Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: failed to read \($report)"]} else
      {tests: [$tests[] | del(.collection_error)]}
      + ([$tests[] | select(.collection_error) | "pytest collection failed: \(.test_case.path)"] | last | if . then {error: .} else {} end)
    end
    | if $code == 2 and (.error // "") == "" then .error = $error else . end'
}

bk_serve
