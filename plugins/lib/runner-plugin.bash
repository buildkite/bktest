# shellcheck shell=bash
# Bash SDK for bktest runner plugins. Requires Bash 4.4+ and jq 1.6+.
#
# A plugin sources this file, defines a function for each operation it
# implements, and ends with `bk_main "$@"`:
#
#   plugin_configure           plugin_prepare_attempt     (required)
#   plugin_discover_targets    plugin_discover_examples
#   plugin_interpret_attempt   plugin_parse_batch_report
#
# bktest starts the plugin once per operation, as `plugin.bash <operation>`,
# with the request as JSON on stdin. bk_main saves the request to the file
# $BK_REQUEST and calls the operation's function, which prints the response
# as JSON on stdout or calls bk_fail with a message. bk_main adds the
# protocol version and, to the configure response, the optional operations
# the plugin defines.
#
# Read the request with bk_request, which runs a jq filter over it. Requests
# can list thousands of test cases, so never copy one into a variable or a
# command's arguments: build the response from it with a jq filter.
#
# Nothing is kept in memory between operations. Keep what a later operation
# needs in files under $BK_STATE_DIR, which lasts for the bktest command.
#
# stdout carries only the response: send everything else, such as the output
# of dry runs, to stderr. See docs/runner-plugins.md for the protocol.

set -euo pipefail

BK_PROTOCOL_VERSION=1

# jq functions available to bk_request filters. Their input is the request,
# unless noted.
# shellcheck disable=SC2016
BK_JQ_DEFS='
# The words of a command string, split like a POSIX shell without expanding
# anything. A word is a run of unquoted characters, single-quoted strings,
# double-quoted strings and backslash escapes. Spaces, tabs and newlines
# separate words. A backslash before a newline continues the line.
def bk_words:
  "[^ \\t\\n\u0027\"\\\\]+|\u0027[^\u0027]*\u0027|\"(?:[^\"\\\\]|\\\\[\\s\\S])*\"" as $piece
  | "(?:\($piece)|\\\\[^\\n])(?:\($piece)|\\\\[\\s\\S])*" as $word
  | if ([scan($word + "|(?:[ \\t\\n]|\\\\\\n)+")] | map(length) | add // 0) != length
    then error("failed to split command: unterminated quote or escape in: " + .)
    else . end
  | [scan($word)
     | [scan("\u0027([^\u0027]*)\u0027|\"((?:[^\"\\\\]|\\\\[\\s\\S])*)\"|\\\\([\\s\\S])|([^\u0027\"\\\\]+)")
        | if .[0] != null then .[0]
          elif .[1] != null then .[1] | gsub("\\\\(?<c>[\\\\\"$`\n])"; .c | if . == "\n" then "" else . end)
          elif .[2] != null then .[2] | if . == "\n" then "" else . end
          else .[3] end]
     | join("")];

# The test command, or the retry command when the request is a retry.
def bk_test_command:
  if .retry then .config.retry_test_command else .config.test_command end;

# What the framework runs for each test case: the selector value for selector
# test cases, otherwise the path.
def bk_paths:
  [.test_cases[] | if .format == "selector" then .value else .path end];

# The position of the first word equal to $placeholder in the input array.
def bk_index($placeholder):
  first(range(length) as $i | select(.[$i] == $placeholder) | $i) // null;

# The input array with its first $placeholder word replaced by the $values
# array, or unchanged if it has no such word.
def bk_splice($placeholder; $values):
  bk_index($placeholder) as $i
  | if $i == null then . else .[:$i] + $values + .[$i + 1:] end;

# The input object with $key set to value, unless it is already set. value is
# evaluated against the input, so one default can refer to another setting.
def bk_default($key; value):
  if (.[$key] // "") == "" then .[$key] = value else . end;

# The input array or string, or an empty one for null, as Go reads JSON into
# a field of that type. Another type is an error, so a report of an
# unexpected shape is rejected instead of misread.
def bk_array:
  if type == "array" then . elif . == null then [] else error("expected an array, not \(type)") end;
def bk_string:
  if type == "string" then . elif . == null then "" else error("expected a string, not \(type)") end;

# The strings of the input array joined by $separator. Unlike join, which
# copies the string so far for each element, it is fast for thousands of
# elements.
def bk_join($separator):
  if length == 0 then "" else .[0] + (.[1:] | map($separator + .) | add // "") end;

# The elements of the input path, without the empty and "." elements, and
# without the elements that a ".." element leaves.
def bk_path_elements:
  startswith("/") as $absolute
  | reduce (split("/")[] | select(. != "" and . != ".")) as $element ([];
      if $element != ".." then . + [$element]
      elif length > 0 and .[-1] != ".." then .[:-1]
      elif $absolute then .
      else . + [$element] end);

# The input path relative to the directory $base, as filepath.Rel in Go
# returns it. Reports name files by absolute path, and bktest names them
# relative to the working directory. It is an error if only one of the two
# paths is absolute.
def bk_rel($base):
  . as $target
  | "Rel: can\u0027t make \($target) relative to \($base)" as $impossible
  | if startswith("/") != ($base | startswith("/")) then error($impossible) else . end
  | bk_path_elements as $path
  | ($base | bk_path_elements) as $from
  | ([($path | length), ($from | length)] | min) as $shortest
  | (first(range(0; $shortest) | select($path[.] != $from[.])) // $shortest) as $common
  | if $from[$common] == ".." then error($impossible) else . end
  | [range($common; $from | length) | ".."] + $path[$common:]
  | if length == 0 then "." else join("/") end;

# The input string with the special characters of regular expressions
# escaped, as regexp.QuoteMeta in Go returns it.
def bk_regex_quote:
  reduce ("\\", ".", "+", "*", "?", "(", ")", "|", "[", "]", "{", "}", "^", "$") as $special (.;
    split($special) | join("\\" + $special));

# The input string cut around its first $separator, as [before, after], or
# null if it has no $separator.
def bk_cut($separator):
  split($separator)
  | if length < 2 then null else [.[0], (.[1:] | join($separator))] end;

# The input string with its first $placeholder replaced by $value.
def bk_replace_first($placeholder; $value):
  bk_cut($placeholder) as $parts
  | if $parts == null then . else $parts[0] + $value + $parts[1] end;

# The functions below split a command string that has a list put into it
# before it is split, as $prefix, the list and $suffix. bk_words takes seconds
# for a string of thousands of words, so they split $prefix and $suffix and
# take the words in between from the list. When they can not, such as when
# $prefix ends inside a quote, they split the whole string.

# $middle is the words of "x" + text + "x", or null. A word can start in
# $prefix and end in $suffix, and the "x" tells such a word apart from no
# word. text is only evaluated to split the whole string.
def _bk_words_around($prefix; $middle; text; $suffix):
  if $middle != null and ($prefix | try (bk_words | true) catch false) then
    ($prefix + "x" | bk_words) as $before
    | ("x" + $suffix | bk_words) as $after
    | ($middle | length) as $n
    | (if $n == 1 then [$before[-1][:-1] + $middle[0][1:-1] + $after[0][1:]]
       else [$before[-1][:-1] + $middle[0][1:]] + $middle[1:$n - 1] + [$middle[$n - 1][:-1] + $after[0][1:]] end) as $words
    # An empty first or last word may be an empty quoted word or no word.
    | if $words[0] == "" or $words[-1] == "" then null else $before[:-1] + $words + $after[1:] end
  else null end
  | if . == null then $prefix + text + $suffix | bk_words else . end;

# The words of $prefix + text + $suffix, where text is $words quoted for a
# shell and joined with spaces, so that it splits into $words.
def bk_words_around($prefix; $words; text; $suffix):
  _bk_words_around($prefix;
    ($words | if length == 0 then null else ["x" + .[0]] + .[1:] | .[:-1] + [.[-1] + "x"] end);
    text; $suffix);

# The words of $prefix + ($values | bk_join(" ")) + $suffix, where $values
# are strings that are not quoted, so one with a space is more than one word.
def bk_words_joined($prefix; $values; $suffix):
  ($values | length) as $n
  | _bk_words_around($prefix;
      (if $n == 0 then null else
         [range($n) as $i
          | $values[$i]
          | (if $i == 0 then "x" else "" end) as $first
          | (if $i == $n - 1 then "x" else "" end) as $last
          | if . != "" and (test("[ \\t\\n\u0027\"\\\\]") | not) then [$first + . + $last]
            elif test("[\u0027\"\\\\]") | not then [$first + . + $last | scan("[^ \\t\\n]+")]
            # A value with an open quote can not be split on its own.
            elif try (bk_words | false) catch true then null
            else $first + . + $last | bk_words end]
         | if any(.[]; . == null) then null else [.[][]] end
       end);
      $values | bk_join(" "); $suffix);

# The JSON document in the file given to `jq -n`. As in Go, a file with no
# document, or with more than one, is an error.
def bk_document:
  [inputs]
  | if length == 0 then error("unexpected end of JSON input")
    elif length > 1 then error("invalid character \u0027\(.[1] | tojson | .[:1])\u0027 after top-level value")
    else .[0] end;

# What Go calls the type of the input in its JSON errors.
def bk_go_type: if type == "boolean" then "bool" else type end;

# The tests of a document in Test Engine JSON format, as the Buildkite test
# collectors write, as an array of {id, name, scope, location, file_name,
# result, tags} with a string for every missing attribute. Errors are worded
# as bktest words them for its Go runners.
def bk_test_engine_tests:
  def mismatch($field; $go_type):
    error("json: cannot unmarshal \(bk_go_type) into Go struct field \($field) of type \($go_type)");
  # field is only evaluated for the error.
  def text(field; $go_type):
    if . == null then "" elif type == "string" then . else mismatch(field; $go_type) end;
  if . == null then []
  elif type != "array" then error("json: cannot unmarshal \(bk_go_type) into Go value of type []runner.TestEngineTest")
  else . end
  | to_entries
  | map(.key as $i
      | .value
      | if . == null then {}
        elif type != "object" then error("json: cannot unmarshal \(bk_go_type) into .\($i) of type runner.TestEngineTest")
        else . end
      | {
          id: (.id | text(".\($i).id"; "string")),
          name: (.name | text(".\($i).name"; "string")),
          scope: (.scope | text(".\($i).scope"; "string")),
          location: (.location | text(".\($i).location"; "string")),
          file_name: (.file_name | text(".\($i).file_name"; "string")),
          result: (.result | text(".\($i).result"; "runner.TestStatus")),
          tags: (.tags
            | if . == null then {}
              elif type != "object" then mismatch(".\($i).tags"; "map[string]string")
              elif all(.[]; type == "string") then .
              else with_entries(.key as $tag | .value |= text(".\($i).tags.\($tag)"; "string")) end)
        });
'

# bk_fail fails the operation with a message bktest shows.
bk_fail() {
  printf '%s' "$*" >"$BK_TMP/error"
  exit 1
}

# bk_request FILTER [JQ_OPTION...] runs a jq filter over the request.
bk_request() {
  local filter=$1
  shift
  if ! jq "$@" "$BK_JQ_DEFS $filter" "$BK_REQUEST" 2>"$BK_TMP/jq-error"; then
    bk_fail "$(sed -e 's/^jq: error ([^)]*)\( (not a string)\)\{0,1\}: //' "$BK_TMP/jq-error")"
  fi
}

# bk_report FILE FILTER [JQ_OPTION...] runs a jq filter over the JSON value in
# a file, such as the report of a test command, which can be megabytes. The
# filter can use the jq functions above, and the request as $request[0] with
# the options --slurpfile request "$BK_REQUEST". If it fails, it prints
# nothing, sets BK_REPORT_ERROR to the reason, and returns 1 if the file
# can't be read, worded like Go's os.ReadFile, or 2 if the file isn't one
# JSON value or the filter failed.
bk_report() {
  local file=$1 filter=$2
  shift 2
  BK_REPORT_ERROR=
  if [[ ! -e $file ]]; then
    BK_REPORT_ERROR="open $file: no such file or directory"
  elif [[ -d $file ]]; then
    BK_REPORT_ERROR="read $file: is a directory"
  elif [[ ! -r $file ]]; then
    BK_REPORT_ERROR="open $file: permission denied"
  fi
  [[ -z $BK_REPORT_ERROR ]] || return 1
  # shellcheck disable=SC2016
  if jq --slurp "$@" "$BK_JQ_DEFS"'
      if length == 1 then .[0]
      elif length == 0 then error("unexpected end of JSON input")
      else error("unexpected data after the JSON value") end
      | '"$filter" "$file" >"$BK_TMP/report-output" 2>"$BK_TMP/jq-error"; then
    cat "$BK_TMP/report-output"
    return 0
  fi
  BK_REPORT_ERROR=$(sed -e 's/^jq: //' -e 's/^parse error: //' \
    -e 's/^error ([^)]*)\( (not a string)\)\{0,1\}: //' "$BK_TMP/jq-error")
  return 2
}

# bk_cfg prints a setting from the request's config, or an empty string.
bk_cfg() {
  jq -r --arg key "$1" '.config[$key] // "" | tostring' "$BK_REQUEST"
}

# bk_log writes a debug log to stderr when bktest runs with --debug.
bk_log() {
  if [[ $BK_DEBUG == true ]]; then
    printf 'DEBUG: %s\n' "$*" >&2
  fi
}

# bk_argv ARRAY FILTER reads the array of strings FILTER produces from the
# request into the Bash array named ARRAY, to run a command with, such as a
# dry run. Unlike a response, a command's arguments are limited in size by
# the operating system.
bk_argv() {
  local -n _bk_argv=$1
  bk_request "$2 | .[] + \"\u0000\"" -j >"$BK_TMP/argv"
  mapfile -d '' _bk_argv <"$BK_TMP/argv"
}

# bk_junit_json prints the test cases of a JUnit XML report as a JSON array
# of {classname, name, result, suite, failure}. It needs python3, because
# neither Bash nor jq can parse XML.
bk_junit_json() {
  python3 - "$1" <<'PY'
import json, sys, xml.etree.ElementTree as ET
try:
    root = ET.parse(sys.argv[1]).getroot()
except Exception as error:
    sys.exit(str(error))
if root.tag != "testsuites":
    sys.exit("expected element type <testsuites> but have <%s>" % root.tag)
cases = []
for suite in root.findall("testsuite"):
    for case in suite.findall("testcase"):
        failure = case.find("failure")
        if failure is not None or case.find("error") is not None:
            result = "failed"
        elif case.find("skipped") is not None:
            result = "skipped"
        else:
            result = "passed"
        cases.append({
            "classname": case.get("classname", ""),
            "name": case.get("name", ""),
            "result": result,
            "suite": suite.get("name", ""),
            "failure": (failure.text or "") if failure is not None else None,
        })
json.dump(cases, sys.stdout)
PY
}

# The helpers below word their errors as bktest words them for its Go
# runners, for the messages a plugin reports about a report it can't read.

# bk_check_file PATH returns 1 and prints the reason if PATH can't be read.
bk_check_file() {
  if [[ ! -e $1 ]]; then
    printf 'open %s: no such file or directory' "$1"
  elif [[ -d $1 ]]; then
    printf 'read %s: is a directory' "$1"
  elif [[ ! -r $1 ]]; then
    printf 'open %s: permission denied' "$1"
  else
    return 0
  fi
  return 1
}

# bk_jq_error prints the message of the error jq wrote to $BK_TMP/jq-error,
# such as an error of bk_document, or of a JSON document that ends early.
bk_jq_error() {
  sed -n -e 's/^jq: error ([^)]*)\( (not a string)\)\{0,1\}: //' \
    -e 's/^Unfinished [A-Za-z ]* at EOF at line [0-9]*, column [0-9]*.*$/unexpected end of JSON input/' \
    -e 's/ (while parsing .*$//' \
    -e '1p' "$BK_TMP/jq-error"
}

# bk_junit_read REPORT OUT writes the test cases of a JUnit XML report to
# the file OUT, as bk_junit_json prints them, or returns 1 and prints the
# reason.
bk_junit_read() {
  local reason
  if ! reason=$(bk_check_file "$1"); then
    case $reason in
      read*) printf 'failed to read JUnit XML file %s: %s' "$1" "$reason" ;;
      *) printf 'failed to open JUnit XML file %s: %s' "$1" "$reason" ;;
    esac
    return 1
  fi
  if ! reason=$(bk_junit_json "$1" 2>&1 >"$2"); then
    # Go reports a document without an element as the end of the file.
    grep -q '<[^!?]' "$1" || reason=EOF
    printf 'failed to unmarshal JUnit XML file %s: %s' "$1" "$reason"
    return 1
  fi
}

_bk_operations() {
  local operation
  for operation in discover-targets discover-examples interpret-attempt parse-batch-report; do
    if declare -F "plugin_${operation//-/_}" >/dev/null; then
      printf '%s\n' "$operation"
    fi
  done
}

# bk_main runs the operation named by its argument.
bk_main() {
  if [[ $# -ne 1 ]]; then
    printf 'usage: %s <operation>\n\nThis is a bktest runner plugin. Operations read a JSON request from stdin and write a JSON response to stdout.\n' "$0" >&2
    exit 2
  fi
  if ! command -v jq >/dev/null; then
    echo "bktest runner plugin: jq is required" >&2
    exit 1
  fi

  local operation=$1 handler status=0 message
  handler="plugin_${operation//-/_}"
  BK_TMP=$(mktemp -d "${TMPDIR:-/tmp}/bktest-plugin.XXXXXX")
  trap 'rm -rf -- "$BK_TMP"' EXIT
  BK_REQUEST="$BK_TMP/request.json"
  cat >"$BK_REQUEST"

  if ! declare -F "$handler" >/dev/null; then
    jq -cn --arg operation "$operation" '{error: "this plugin does not implement \($operation)"}'
    exit 1
  fi
  if ! BK_STATE_DIR=$(jq -er '.state_dir // ""' "$BK_REQUEST" 2>/dev/null); then
    jq -cn --arg operation "$operation" '{error: "invalid \($operation) request"}'
    exit 1
  fi
  BK_DEBUG=$(jq -r '.config.debug // false' "$BK_REQUEST")
  export BK_TMP BK_REQUEST BK_STATE_DIR BK_DEBUG

  # A subshell that isn't part of a condition, so errexit applies in the
  # handler.
  set +e
  (
    set -e
    "$handler"
  ) >"$BK_TMP/response" </dev/null
  status=$?
  set -e

  if [[ $status -eq 0 ]] && jq -c \
    --argjson version "$BK_PROTOCOL_VERSION" \
    --arg configure "$([[ $operation == configure ]] && _bk_operations)" \
    '. + {protocol_version: $version}
     + (if $configure != "" then {operations: ($configure | split("\n"))} else {} end)' \
    "$BK_TMP/response" 2>/dev/null; then
    exit 0
  fi
  if [[ -s "$BK_TMP/error" ]]; then
    message=$(<"$BK_TMP/error")
  elif [[ $status -eq 0 ]]; then
    message="$operation printed an invalid response"
  else
    message="$operation failed"
  fi
  # The message can be longer than an argument of a command can be.
  printf '%s' "$message" | jq -Rsc '{error: .}'
  exit 1
}
