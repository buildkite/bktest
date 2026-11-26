#!/usr/bin/env bash
# Bash runner plugin for pytest. It reads the JSON that the Buildkite test
# collector writes when its Python package is installed, and JUnit XML
# otherwise. JUnit XML is parsed with python3.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

# shellcheck disable=SC2016
PYTEST_DEFS='
# The input string quoted as one word of a shell command, with a backslash
# before each special character, or in single quotes if it has whitespace.
def pytest_quote:
  if . == "" then "\u0027\u0027"
  elif test("[ \\t\\n]") then
    split("\u0027")
    | map(if . == "" then "" else "\u0027" + . + "\u0027" end)
    | join("\\\u0027")
  elif test("[\\\\\u0027\"`${\\[|&;<>()*?!]|^~") then
    gsub("(?<c>[\\\\\u0027\"`${\\[|&;<>()*?!])"; "\\" + .c) | sub("^~"; "\\~")
  else . end;

# The format of the report that the report option of the input command asks
# for, or "" if it has no report option.
def pytest_option_format:
  if contains("--junit-xml") then "junit" elif contains("--json=") then "json" else "" end;

# The test command for the test cases. The quoted paths and the result path
# are put into the command before it is split into words, so a placeholder
# can be part of a word or inside quotes.
def pytest_argv:
  "{{resultPath}}" as $placeholder
  | (.config.result_path // "") as $result_path
  | (bk_paths | map(. // "")) as $paths
  | (bk_test_command // "") as $command
  | ($command | bk_cut("{{testExamples}}") // [$command + " ", ""]) as [$before, $after]
  | if ($before | contains($placeholder) | not) and any($paths[]; contains($placeholder)) then
      # The first result path placeholder may be in a path. That is not worth
      # doing fast.
      $before + ($paths | map(pytest_quote) | bk_join(" ")) + $after
      | bk_replace_first($placeholder; $result_path)
      | bk_words
    else
      (if $before | contains($placeholder)
       then [($before | bk_replace_first($placeholder; $result_path)), $after]
       else [$before, ($after | bk_replace_first($placeholder; $result_path))] end) as [$prefix, $suffix]
      | bk_words_around($prefix; $paths; $paths | map(pytest_quote) | bk_join(" "); $suffix)
    end;

# The scope the JUnit XML report has for a scope of the test collector: the
# classname, which is the file path without ".py" and the classes, with dots.
def pytest_junit_scope:
  split("::")
  | [(.[0] // "" | if endswith(".py") then .[:-3] else . end)] + .[1:]
  | join(".") | split("/") | join(".");

# The node ID of a test case of the JUnit XML report. The classname is the
# module and any classes, with dots. The module ends at the last part named
# like a test file, or else before the first part that starts with a capital.
def pytest_node_id:
  .name as $name
  | if .classname == "" then $name else
      (.classname | split(".")) as $parts
      | ([range($parts | length) | select($parts[.] | startswith("test_") or endswith("_test")) | . + 1] | last) as $named
      | ($named
         // ([range($parts | length) | select($parts[.][:1] | . >= "A" and . <= "Z")] | .[0])
         // ($parts | length)) as $stop
      | ($parts[:$stop] | join("/")) + ".py" + ($parts[$stop:] | map("::" + .) | join("")) + "::" + $name
    end;

# The path of a test of the collector report: its node ID.
def pytest_path: if .scope == "" then .name else .scope + "::" + .name end;

def pytest_collection_error: .tags["test.pytest_collection_error"] == "true";

# The response for a pool batch report from its tests, as {test_case,
# status} in the order of the report. pytest can report a test twice, such as
# when it fails and its teardown errors: it is then reported once, failed if
# any report failed.
def pytest_batch_tests:
  map(.status as $status
      | if $status != "passed" and $status != "failed" and $status != "skipped"
        then error("unknown pytest test status \($status | tojson)") else . end
      | (.test_case.path | split("::") | .[0] // "") as $file
      | if $file == "" or .test_case.name == ""
        then error("pytest test \(.test_case.path | tojson) has no file or name") else . end
      | {test_case: (.test_case + {identifier: .test_case.path}), status: $status, selector: $file})
  # Group the reports of each test, then put the tests back in order.
  | to_entries
  | group_by(.value.test_case.path)
  | map(.[0].key as $position
      | reduce .[1:][].value.status as $status (.[0].value;
          if $status == "failed" or .status == "skipped" then .status = $status else . end)
      | {$position, test: .})
  | sort_by(.position)
  | map(.test);
'

# pytest_collector_installed reports whether the Buildkite test collector
# can be imported. `bktest pool run`'s persistent runner is part of it.
pytest_collector_installed() {
  [[ $(bk_cfg persistent_runner) == true ]] ||
    [[ $(python -c 'import importlib.util, sys; print(importlib.util.find_spec(sys.argv[1]) is not None)' buildkite_test_collector 2>/dev/null) == True ]]
}

# pytest_check_collector_version prints why the test collector can't filter
# by tag, which needs a version after 1.2.0, and returns 1.
pytest_check_collector_version() {
  local version error
  if ! version=$(python -c 'import importlib.metadata, sys; print(importlib.metadata.version(sys.argv[1]))' buildkite-test-collector 2>/dev/null); then
    error='exit status 1'
    # shellcheck disable=SC2016
    command -v python >/dev/null || error='exec: "python": executable file not found in $PATH'
    printf 'could not determine buildkite-test-collector version: %s' "$error"
    return 1
  fi
  # A semantic version: v1, v1.2 or v1.2.3, with a prerelease and a build.
  local number='(0|[1-9][0-9]*)' rest='(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?'
  if [[ ! $version =~ ^$number(\.$number(\.$number$rest)?)?$ ]]; then
    printf 'could not parse installed buildkite-test-collector version "%s"' "$version"
    return 1
  fi
  local major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[3]:-0} patch=${BASH_REMATCH[5]:-0}
  # A prerelease of a version is before the version, so only these count.
  if ((major > 1 || (major == 1 && (minor > 2 || (minor == 2 && patch > 0))))); then
    return 0
  fi
  printf 'buildkite-test-collector version %s is installed, but version >1.2.0 is required for --tag-filters support' "$version"
  return 1
}

# pytest_format prints the format of the report, junit or json. The test
# command decides it when it has a report option, and otherwise configure
# does, and keeps it in the state directory.
pytest_format() {
  local format
  format=$(bk_request "$PYTEST_DEFS"'.config.test_command // "" | pytest_option_format' -r)
  if [[ -n $format ]]; then
    echo "$format"
  elif [[ -r $BK_STATE_DIR/pytest-format ]]; then
    cat "$BK_STATE_DIR/pytest-format"
  elif pytest_collector_installed; then
    echo json
  else
    echo junit
  fi
}

plugin_configure() {
  local installed=false format extension=xml command='pytest {{testExamples}} --junit-xml={{resultPath}}' result_path='' reason
  if pytest_collector_installed; then
    installed=true
    extension=json
    command='pytest {{testExamples}} --json={{resultPath}}'
  fi

  if [[ -n $(bk_cfg tag_filters) ]]; then
    if [[ $installed != true ]]; then
      bk_fail "--tag-filters requires the 'buildkite-test-collector' package."$'\n''Please install it with: pip install buildkite-test-collector.'
    elif ! reason=$(pytest_check_collector_version); then
      bk_fail "$reason"$'\n''Please upgrade with: pip install --upgrade buildkite-test-collector'
    fi
  fi

  # A report option in the command decides the format, whether or not the
  # test collector is installed.
  # shellcheck disable=SC2016
  format=$(bk_request "$PYTEST_DEFS"'.config | bk_default("test_command"; $command) | .test_command | pytest_option_format' \
    -r --arg command "$command")
  if [[ $format == json && $installed != true ]]; then
    bk_fail "--json requires the 'buildkite-test-collector' package to be installed."$'\n''Please install it with: pip install buildkite-test-collector.'
  elif [[ -z $format && $installed == true ]]; then
    format=json
  elif [[ -z $format ]]; then
    format=junit
  fi
  if [[ $format == junit ]]; then
    echo 'Buildkite Test Engine Client: Using JUnit XML output for pytest results.' >&2
  else
    echo 'Buildkite Test Engine Client: Using JSON output for pytest results.' >&2
  fi
  if [[ -d $BK_STATE_DIR ]]; then
    echo "$format" >"$BK_STATE_DIR/pytest-format"
  fi

  if [[ -z $(bk_cfg result_path) ]]; then
    result_path="$(mktemp -d "${TMPDIR:-/tmp}/bktec-pytest-XXXXXX")/pytest-results.$extension"
  fi

  # The test collector uploads its own results, so bktest only uploads JUnit
  # XML. Pool batch reports are read in either format.
  # shellcheck disable=SC2016
  bk_request '{
    name: "pytest",
    config: (.config
      | bk_default("test_command"; $command)
      | bk_default("result_path"; $result_path)
      | bk_default("test_file_pattern"; "**/{*_test,test_*}.py")
      | bk_default("retry_test_command"; .test_command)),
    features: {split_by_file: true, split_by_example: true, filter_test_files: true,
      filter_test_by_tag: true, auto_retry: true, mute: true, skip: true, split_by_selector: true},
    result_format: (if $format == "junit" then "junit" else "" end),
    batch_report_format: "pytest-\($format)"
  }' --arg command "$command" --arg result_path "$result_path" --arg format "$format"
}

plugin_prepare_attempt() {
  bk_request "$PYTEST_DEFS"'{argv: pytest_argv}'
}

# Lists the tests in the files, with the tag filters if there are any. The
# scope and name have to match what the report of the format has.
plugin_discover_examples() {
  local format tag_filters
  local -a argv files
  bk_argv files '.files // []'
  if [[ ${#files[@]} -eq 0 ]]; then
    echo '{"test_cases": []}'
    return
  fi
  format=$(pytest_format)
  tag_filters=$(bk_cfg tag_filters)
  argv=(pytest --collect-only -q)
  if [[ -n $tag_filters ]]; then
    argv+=(--tag-filters "$tag_filters")
  fi
  bk_log "Running \`${argv[*]} ${files[*]}\` to list tests"
  if ! "${argv[@]}" "${files[@]}" >"$BK_TMP/collected" 2>"$BK_TMP/collect-error"; then
    bk_fail "pytest collection failed: $(<"$BK_TMP/collect-error")"
  fi
  # A node ID is a line with "::": the file, any classes and the function.
  # shellcheck disable=SC2016
  jq -Rn --arg format "$format" "$PYTEST_DEFS"'{test_cases: [inputs
    | if test("^\\s|\\s$") then sub("^\\s+"; "") | sub("\\s+$"; "") else . end
    | select(contains("::"))
    | . as $id
    | capture("^(?<scope>.*)::(?<name>.*)$")
    | {
        identifier: $id,
        path: $id,
        scope: (.scope | if $format == "junit" then pytest_junit_scope else . end),
        name: .name,
        format: "example"
      }]}' "$BK_TMP/collected"
}

# pytest exits 1 when tests fail. Other failures leave no report worth
# reading, except that the test collector still reports the files that failed
# to collect when pytest exits 2. The run then has an error, which is the
# command's unless the report says which file failed.
plugin_interpret_attempt() {
  local report format exit_code reason tests="$BK_TMP/tests.json" message=''
  report=$(bk_cfg result_path)
  format=$(pytest_format)
  exit_code=$(bk_request '.exit_code' -r)
  if [[ $exit_code != null && $exit_code != 0 && $exit_code != 1 ]] && [[ $format == junit || $exit_code != 2 ]]; then
    echo '{}'
    return
  fi

  if [[ $format == junit ]]; then
    if ! reason=$(bk_junit_read "$report" "$BK_TMP/junit.json"); then
      message=$reason
    else
      # The scope is the classname, as Test Engine has it for JUnit XML.
      jq "$PYTEST_DEFS"'{tests: map(pytest_node_id as $path | {
        test_case: {identifier: $path, format: "example", scope: .classname, name: .name, path: $path},
        status: .result
      })}' "$BK_TMP/junit.json" >"$tests"
    fi
  elif ! reason=$(bk_check_file "$report"); then
    message="failed to read json: $reason"
  elif ! jq -n "$BK_JQ_DEFS $PYTEST_DEFS"'bk_document | bk_test_engine_tests
    | {tests: map({
        test_case: {identifier: .id, format: "example", scope: .scope, name: .name, path: pytest_path},
        status: .result
      })}
      + ([.[] | select(pytest_collection_error) | pytest_path] | last
         | if . == null then {} else {run_error: "pytest collection failed: \(.)"} end)' \
    "$report" >"$tests" 2>"$BK_TMP/jq-error"; then
    message="failed to parse json: $(bk_jq_error)"
  fi

  if [[ -n $message ]]; then
    jq -n --arg message "Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: $message" \
      '{messages: [$message]}' >"$tests"
  fi
  if [[ $exit_code == 2 ]]; then
    # shellcheck disable=SC2016
    bk_request '$tests[0] + {run_error: ($tests[0].run_error // .error // "")}' --slurpfile tests "$tests"
  else
    cat "$tests"
  fi
}

# Reads the report of a batch for Test Scheduler pools. A file that failed
# to collect has no tests, so it is an errored file in the collector's JSON.
# JUnit XML reports it, and a skipped module, as a test case without a
# classname, which is an error outside tests unless it was skipped.
plugin_parse_batch_report() {
  local format report reason
  format=$(bk_request .format -r)
  report=$(bk_request .report_path -r)
  case $format in
    pytest-json)
      jq -n "$BK_JQ_DEFS $PYTEST_DEFS"'bk_document
        | if . == null then error("not a list of tests") else bk_test_engine_tests end
        | {
            tests: (map(select(pytest_collection_error | not)
              | {test_case: {scope: .scope, name: .name, path: pytest_path}, status: .result}) | pytest_batch_tests),
            errored_files: ([.[] | select(pytest_collection_error) | pytest_path | split("::") | .[0] // ""]
              | [to_entries | group_by(.value)[] | .[0]] | sort_by(.key) | map(.value))
          }' "$report" 2>"$BK_TMP/jq-error" ||
        bk_fail "invalid pytest JSON report: $(bk_jq_error)"
      ;;
    pytest-junit)
      if ! reason=$(bk_junit_read "$report" "$BK_TMP/junit.json"); then
        bk_fail "invalid pytest JUnit XML report: ${reason#"failed to unmarshal JUnit XML file $report: "}"
      fi
      jq "$PYTEST_DEFS"'{
        tests: (map(select(.classname != "")
          | {test_case: {scope: .classname, name: .name, path: pytest_node_id}, status: .result}) | pytest_batch_tests),
        errors_outside_tests: any(.[]; .classname == "" and .result != "skipped")
      }' "$BK_TMP/junit.json" 2>"$BK_TMP/jq-error" ||
        bk_fail "invalid pytest JUnit XML report: $(bk_jq_error)"
      ;;
    *)
      bk_fail "unsupported runner report format \"$format\""
      ;;
  esac
}

bk_main "$@"
