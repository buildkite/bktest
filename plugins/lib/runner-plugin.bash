# shellcheck shell=bash
# Bash SDK for bktest runner plugins. Requires Bash 4+ and jq 1.6+.
#
# Source this file, define handlers, then call bk_serve:
#
#   plugin_initialize  plugin_discover  plugin_examples
#   plugin_command     plugin_interpret
#
# bk_serve reads one JSON-RPC request per line from stdin and calls the
# handler for its method with the request's params in $BK_PARAMS. A handler
# prints its result as JSON on stdout, or calls bk_fail with a message.
# Handlers run in a subshell, so keep state between calls in files under
# $BK_STATE_DIR, such as the effective config bk_save_config writes.
#
# stdout is the protocol channel: send everything else, such as the output
# of dry runs, to stderr.

if [[ -z "${BKTEST_RUNNER_PLUGIN_PROTOCOL:-}" ]]; then
  echo "This is a bktest runner plugin. Run it with \`bktest run --plugin <directory>\`, not directly." >&2
  exit 1
fi
if ! command -v jq >/dev/null; then
  echo "bktest runner plugin: jq is required" >&2
  exit 1
fi

BK_STATE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/bktest-plugin.XXXXXX")"
trap 'rm -rf -- "$BK_STATE_DIR"' EXIT
BK_PARAMS='{}'

# bk_fail fails the current request with a message bktest shows.
bk_fail() {
  printf '%s' "$*" >"$BK_STATE_DIR/error"
  exit 1
}

# bk_log writes a debug log to stderr when bktest runs with --debug.
bk_log() {
  if [[ "$(bk_cfg debug)" == "true" ]]; then
    printf 'DEBUG: %s\n' "$*" >&2
  fi
}

# bk_save_config saves the effective config JSON from stdin for bk_cfg.
bk_save_config() {
  jq -c . >"$BK_STATE_DIR/config.json"
}

# bk_cfg prints a config value, or an empty string.
bk_cfg() {
  jq -r --arg key "$1" 'if .[$key] == null then "" else .[$key] | tostring end' "$BK_STATE_DIR/config.json" 2>/dev/null
}

# bk_param prints a jq expression over the request params.
bk_param() {
  jq -r "$1" <<<"$BK_PARAMS"
}

# bk_paths prints what the framework runs for each test case, one per line:
# the selector value for selector test cases, otherwise the path.
bk_paths() {
  jq -r '.test_cases[] | if .format == "selector" then .value else .path end' <<<"$BK_PARAMS"
}

# bk_json_array prints its arguments as a JSON array of strings.
bk_json_array() {
  # The "x" prefix stops jq parsing arguments such as --tag as options. NUL
  # separators would break on jq 1.6, whose split and regexes mishandle them.
  jq -nc '$ARGS.positional | map(.[1:])' --args "${@/#/x}"
}

# bk_command_result prints a command result that runs its arguments.
bk_command_result() {
  bk_json_array "$@" | jq -c '{argv: .}'
}

# bk_test_command splits the test command, or the retry command on retries,
# into the array named by $1.
bk_test_command() {
  local -n _cmd_words=$1
  local cmd
  if [[ $(bk_param .retry) == true ]]; then cmd=$(bk_cfg retry_test_command); else cmd=$(bk_cfg test_command); fi
  bk_words _cmd_words "$cmd"
}

# bk_splice ARRAY PLACEHOLDER [VALUE...] replaces the first word of ARRAY
# that is PLACEHOLDER with the values. It returns 1 if there is none.
bk_splice() {
  local -n _words=$1
  local placeholder=$2 i
  shift 2
  for i in "${!_words[@]}"; do
    if [[ ${_words[i]} == "$placeholder" ]]; then
      _words=("${_words[@]:0:i}" "$@" "${_words[@]:i+1}")
      return 0
    fi
  done
  return 1
}

# bk_words ARRAY COMMAND splits COMMAND into ARRAY like a POSIX shell,
# without expanding anything, or fails the request.
bk_words() {
  local -n _bk_words=$1
  bk_split_words "$2" >"$BK_STATE_DIR/words" || exit 1
  mapfile -d '' _bk_words <"$BK_STATE_DIR/words"
}

# bk_split_words prints the words of a command, NUL-terminated.
bk_split_words() {
  # A word is a run of unquoted characters, '...', "..." and \x. Fail if the
  # words and spaces don't cover the whole command, such as an open quote.
  # shellcheck disable=SC2016
  printf '%s' "$1" | jq -Rsj '
    "(?:[^\\s\u0027\"\\\\]+|\u0027[^\u0027]*\u0027|\"(?:[^\"\\\\]|\\\\.)*\"|\\\\.)+" as $word
    | if ([scan($word + "|\\s+")] | map(length) | add // 0) != length then error("Unterminated quoted string") else . end
    | [scan($word)
       | [scan("\u0027([^\u0027]*)\u0027|\"((?:[^\"\\\\]|\\\\.)*)\"|\\\\(.)|([^\u0027\"\\\\]+)")
          | if .[0] != null then .[0]
            elif .[1] != null then .[1] | gsub("\\\\(?<c>[\\\\\"$`\n])"; .c)
            elif .[2] != null then .[2]
            else .[3] end]
       | join("")]
    | .[] + "\u0000"' || bk_fail "failed to split command: $1"
}

# bk_shell_join quotes its arguments into one shell-safe string.
bk_shell_join() {
  local out='' arg
  for arg in "$@"; do
    if [[ $arg =~ ^[A-Za-z0-9_./:=@%+,-]+$ ]]; then
      out+="${out:+ }$arg"
    else
      out+="${out:+ }'${arg//\'/\'\\\'\'}'"
    fi
  done
  printf '%s' "$out"
}

# bk_glob prints the regular files matching a glob pattern with ** and {a,b},
# minus those matching the exclude pattern or under node_modules, sorted.
# It uses Bash's globstar, so its matching is close to, not the same as,
# bktest's built-in discovery.
bk_glob() {
  local pattern=$1 exclude=${2:-}
  if [[ $pattern == *[\$\`\;\&\|\<\>]* || $exclude == *[\$\`\;\&\|\<\>]* ]]; then
    bk_fail "unsupported characters in test file pattern"
  fi
  (
    shopt -s globstar nullglob extglob dotglob
    local -a matches excludes=()
    local file prefix ex
    eval "matches=($pattern)"
    if [[ -n $exclude ]]; then
      set -o noglob
      eval "excludes=($exclude)"
      set +o noglob
    fi
    for file in "${matches[@]}"; do
      [[ -f $file ]] || continue
      [[ /$file/ == */node_modules/* ]] && continue
      for ex in "${excludes[@]}"; do
        # In [[ ]], * also matches /, and **/ may match no directories.
        ex=${ex//\*\*\//@(*/|)}
        prefix=$file
        while :; do
          # shellcheck disable=SC2053
          [[ $prefix == $ex ]] && continue 3
          [[ $prefix == */* ]] || break
          prefix=${prefix%/*}
        done
      done
      printf '%s\n' "$file"
    done | LC_ALL=C sort -u
  )
}

# bk_discover_files prints a discover result for the configured patterns.
bk_discover_files() {
  local pattern exclude files
  pattern=$(bk_cfg test_file_pattern)
  exclude=$(bk_cfg test_file_exclude_pattern)
  bk_log "Discovering test files with include pattern: $pattern exclude pattern: $exclude"
  files=$(bk_glob "$pattern" "$exclude") || exit 1
  if [[ -z $files ]]; then
    bk_fail "no files found with pattern \"$pattern\" and exclude pattern \"$exclude\""
  fi
  jq -Rn '{targets: [inputs]}' <<<"$files"
}

# bk_relpath prints a path relative to the working directory.
bk_relpath() {
  case $1 in
    "$PWD"/*) printf '%s' "${1#"$PWD"/}" ;;
    /*) realpath -m --relative-to="$PWD" "$1" ;;
    *) printf '%s' "$1" ;;
  esac
}

# bk_junit_json prints the test cases of a JUnit XML report as a JSON array
# of {classname, name, result, suite, failure}. It needs python3.
bk_junit_json() {
  python3 - "$1" <<'PY'
import json, sys, xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
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

bk_respond() {
  local id=$1 result=$2
  if [[ -n $result ]] && jq -e . >/dev/null 2>&1 <<<"$result"; then
    jq -c --argjson id "$id" '{jsonrpc: "2.0", id: $id, result: .}' <<<"$result"
  else
    bk_respond_error "$id" -32000 "plugin returned an invalid result"
  fi
}

bk_respond_error() {
  jq -cn --argjson id "$1" --argjson code "$2" --arg message "$3" \
    '{jsonrpc: "2.0", id: $id, error: {code: $code, message: $message}}'
}

# bk_serve answers requests until bktest closes stdin.
bk_serve() {
  local line id method handler result
  while IFS= read -r line; do
    id=$(jq -r '.id // 0' <<<"$line" 2>/dev/null) || {
      bk_respond_error 0 -32700 "invalid request"
      continue
    }
    method=$(jq -r '.method' <<<"$line")
    BK_PARAMS=$(jq -c '.params // {}' <<<"$line")
    handler="plugin_$method"
    if ! declare -F "$handler" >/dev/null; then
      bk_respond_error "$id" -32601 "method \"$method\" not found"
      continue
    fi
    rm -f "$BK_STATE_DIR/error"
    if result=$("$handler"); then
      bk_respond "$id" "$result"
    else
      bk_respond_error "$id" -32000 "$(cat "$BK_STATE_DIR/error" 2>/dev/null || echo "$method failed")"
    fi
  done
}
