#!/usr/bin/env bash
# Shared helper library for Bash bktest runner plugins.
# Implements protocol error envelopes, POSIX shell splitting/quoting,
# zzglob-compatible file discovery, relative paths, and JUnit XML decoding.

set -euo pipefail

bktest_err() {
  local msg="$1"
  jq -cn --arg err "$msg" '{protocol_version: 1, error: $err}'
  exit 1
}

bktest_read_req() {
  cat
}

bktest_chdir_from_req() {
  local req="$1"
  local wd
  wd="$(jq -r '.working_dir // ""' <<< "$req")"
  if [[ -n "$wd" ]]; then
    cd "$wd"
  fi
}

# Splits a POSIX shell command string into a JSON array of words, matching
# github.com/kballard/go-shellquote semantics and error strings.
bktest_split_cmd() {
  local cmd_str="$1"
  python3 - "$cmd_str" <<'PY'
import json, sys

s = sys.argv[1]
words = []
i = 0
n = len(s)

def is_space(ch):
    return ch in " \t\n\r"

while i < n:
    while i < n and is_space(s[i]):
        i += 1
    if i >= n:
        break
    buf = []
    while i < n and not is_space(s[i]):
        ch = s[i]
        if ch == "'":
            i += 1
            start = i
            while i < n and s[i] != "'":
                i += 1
            if i >= n:
                sys.stderr.write("Unterminated single-quoted string\n")
                sys.exit(2)
            buf.append(s[start:i])
            i += 1
        elif ch == '"':
            i += 1
            while i < n and s[i] != '"':
                if s[i] == "\\":
                    if i + 1 >= n:
                        sys.stderr.write("Unterminated double-quoted string\n")
                        sys.exit(2)
                    nxt = s[i + 1]
                    if nxt in '$`"\\\n':
                        if nxt != "\n":
                            buf.append(nxt)
                    else:
                        buf.append("\\" + nxt)
                    i += 2
                else:
                    buf.append(s[i])
                    i += 1
            if i >= n:
                sys.stderr.write("Unterminated double-quoted string\n")
                sys.exit(2)
            i += 1
        elif ch == "\\":
            if i + 1 >= n:
                sys.stderr.write("Unterminated backslash-escape\n")
                sys.exit(2)
            nxt = s[i + 1]
            if nxt != "\n":
                buf.append(nxt)
            i += 2
        else:
            buf.append(ch)
            i += 1
    words.append("".join(buf))

if not words:
    sys.stderr.write("test command is empty\n")
    sys.exit(2)

print(json.dumps(words))
PY
}

# Joins a JSON array of words (on stdin) with POSIX shell quoting matching
# github.com/kballard/go-shellquote.Join.
bktest_shellquote_join() {
  python3 -c 'import json, shlex, sys; words = json.load(sys.stdin); print(" ".join(shlex.quote(w) for w in words))'
}

# Discovers files matching a zzglob-compatible include pattern and optional
# exclude pattern, skipping node_modules and excluded directories.
# Outputs a sorted JSON array of paths, or writes an error message to stderr
# and exits non-zero.
bktest_discover_files() {
  local pattern="$1"
  local exclude_pattern="${2:-}"
  python3 - "$pattern" "$exclude_pattern" <<'PY'
import json, os, re, sys

pattern = sys.argv[1]
exclude_pattern = sys.argv[2]

def expand_braces(pat):
    # Recursively expand {a,b,c} respecting nested braces.
    depth = 0
    start = -1
    for idx, ch in enumerate(pat):
        if ch == "{":
            if depth == 0:
                start = idx
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth < 0:
                raise ValueError(f"unmatched '}}' in pattern {pat!r}")
            if depth == 0:
                prefix = pat[:start]
                inner = pat[start + 1:idx]
                suffix = pat[idx + 1:]
                # Split inner on top-level commas
                parts = []
                cur = []
                d = 0
                for c in inner:
                    if c == "{":
                        d += 1
                        cur.append(c)
                    elif c == "}":
                        d -= 1
                        cur.append(c)
                    elif c == "," and d == 0:
                        parts.append("".join(cur))
                        cur = []
                    else:
                        cur.append(c)
                parts.append("".join(cur))
                out = []
                for p in parts:
                    for expanded in expand_braces(prefix + p + suffix):
                        out.append(expanded)
                return out
    if depth != 0:
        raise ValueError(f"unclosed '{{' in pattern {pat!r}")
    return [pat]

def glob_to_regex(pat):
    if pat == "":
        return None
    expanded = expand_braces(pat)
    regexes = []
    for p in expanded:
        p = p.lstrip("./") if p.startswith("./") else p
        i = 0
        n = len(p)
        res = ["^"]
        while i < n:
            if p[i:i+3] == "**/":
                res.append("(?:.*/)?")
                i += 3
            elif p[i:i+2] == "**":
                res.append(".*")
                i += 2
            elif p[i] == "*":
                res.append("[^/]*")
                i += 1
            elif p[i] == "?":
                res.append("[^/]")
                i += 1
            elif p[i] == "[":
                j = i + 1
                if j < n and p[j] in "!^":
                    j += 1
                if j < n and p[j] == "]":
                    j += 1
                while j < n and p[j] != "]":
                    j += 1
                if j >= n:
                    res.append(re.escape("["))
                    i += 1
                else:
                    content = p[i+1:j]
                    if content.startswith("!"):
                        content = "^" + content[1:]
                    res.append("[" + content + "]")
                    i = j + 1
            else:
                res.append(re.escape(p[i]))
                i += 1
        res.append("$")
        regexes.append(re.compile("".join(res)))
    return regexes

def matches(regexes, path):
    if not regexes:
        return False
    return any(r.match(path) for r in regexes)

try:
    inc_re = glob_to_regex(pattern)
except Exception:
    sys.stderr.write(f'error parsing test file pattern "{pattern}"\n')
    sys.exit(2)

try:
    exc_re = glob_to_regex(exclude_pattern)
except Exception:
    sys.stderr.write(f'error parsing test file exclude pattern "{exclude_pattern}"\n')
    sys.exit(2)

found = []
for root, dirs, files in os.walk(".", topdown=True):
    dirs.sort()
    files.sort()
    rel_root = "" if root == "." else root[2:]
    kept_dirs = []
    for d in dirs:
        if d == "node_modules":
            continue
        rel_dir = d if not rel_root else f"{rel_root}/{d}"
        if matches(exc_re, rel_dir):
            continue
        kept_dirs.append(d)
    dirs[:] = kept_dirs

    for f in files:
        rel_file = f if not rel_root else f"{rel_root}/{f}"
        if matches(exc_re, rel_file):
            continue
        if matches(inc_re, rel_file):
            found.append(rel_file)

found.sort()
if not found:
    sys.stderr.write(f'no files found with pattern "{pattern}" and exclude pattern "{exclude_pattern}"\n')
    sys.exit(3)

print(json.dumps(found))
PY
}

# Converts an absolute or relative path into a path relative to base using
# forward slashes (matching Go's filepath.Rel + filepath.ToSlash).
bktest_relpath() {
  local base="$1"
  local target="$2"
  python3 - "$base" "$target" <<'PY'
import os, sys
rel = os.path.relpath(sys.argv[2], sys.argv[1])
print(rel.replace(os.sep, "/"))
PY
}

# Parses JUnit XML from a file path (or "-" for stdin) into a JSON array of
# testcases with suite_name, classname, name, result, and failure_content.
bktest_junit_to_json() {
  local source="$1"
  python3 -c '
import json, sys, xml.etree.ElementTree as ET

src = sys.argv[1]
try:
    if src == "-":
        data = sys.stdin.read()
    else:
        with open(src, "r", encoding="utf-8") as f:
            data = f.read()
except OSError as e:
    sys.stderr.write(f"failed to open JUnit XML file {src}: {e}\n")
    sys.exit(2)

try:
    root = ET.fromstring(data)
except ET.ParseError as e:
    sys.stderr.write(f"failed to unmarshal JUnit XML file {src}: {e}\n")
    sys.exit(2)

if root.tag == "testsuites":
    suites = list(root.findall("testsuite"))
elif root.tag == "testsuite":
    suites = [root]
else:
    sys.stderr.write(f"failed to unmarshal JUnit XML file {src}: unexpected root tag {root.tag}\n")
    sys.exit(2)

results = []
for suite in suites:
    suite_name = suite.attrib.get("name", "")
    for tc in suite.findall("testcase"):
        failure = tc.find("failure")
        error = tc.find("error")
        skipped = tc.find("skipped")
        if failure is not None or error is not None:
            status = "failed"
        elif skipped is not None:
            status = "skipped"
        else:
            status = "passed"
        results.append({
            "classname": tc.attrib.get("classname", ""),
            "name": tc.attrib.get("name", ""),
            "suite_name": suite_name,
            "result": status,
            "has_failure": failure is not None,
            "failure_content": (failure.text or "") if failure is not None else "",
        })

print(json.dumps(results))
' "$source"
}
