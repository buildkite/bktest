#!/usr/bin/env bash
set -euo pipefail

# End-to-end demonstration of both Go and Bash plugin variants for all 9 runners
# using the compiled bktest CLI against real test suites (either this repo's
# internal/runner/testdata or a local checkout of buildkite/bktest-examples).
#
# Usage:
#   examples/plugins/run-examples.sh                           # runs repo testdata suites
#   examples/plugins/run-examples.sh --examples-dir /path/to/bktest-examples
#   examples/plugins/run-examples.sh --runners rspec,vitest    # subset of runners

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXAMPLES_DIR=""
FILTER_RUNNERS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --examples-dir) EXAMPLES_DIR="$2"; shift 2 ;;
    --runners) FILTER_RUNNERS="$2"; shift 2 ;;
    -h|--help)
      sed -n '3,12p' "$0"
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

TMP_DIR="$(mktemp -d /tmp/bktest-plugin-demo-XXXXXX)"
MOCK_PID=""
cleanup() {
  if [[ -n "$MOCK_PID" ]]; then
    kill "$MOCK_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

echo "Building bktest CLI and all 9 Go plugins..."
(cd "$REPO_ROOT" && go build -o "$TMP_DIR/bktest" ./cmd/bktest)
"$REPO_ROOT/bin/build-plugins" >/dev/null

# Start a minimal local mock Buildkite Test Engine HTTP server so `bktest run`
# exercises discovery, planning, execution, and result interpretation offline.
PORT_FILE="$TMP_DIR/mock-port"
cat > "$TMP_DIR/mock_server.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT_FILE = sys.argv[1]
TARGET_OVERRIDE_FILE = sys.argv[2]

class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path.split("?")[0].endswith("/test_plan"):
            self.send_response(404)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"message":"not found"}')
            return
        self.send_response(404)
        self.end_headers()

    def read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            chunks = []
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    continue
                size = int(line.split(b";", 1)[0], 16)
                if size == 0:
                    self.rfile.readline()
                    break
                chunks.append(self.rfile.read(size))
                self.rfile.readline()
            return b"".join(chunks)
        length = int(self.headers.get("Content-Length", "0"))
        return self.rfile.read(length) if length > 0 else b""

    def do_POST(self):
        body = self.read_body()
        path = self.path.split("?")[0]
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        if path.endswith("/filter_tests"):
            self.wfile.write(b'{"tests":[]}')
        elif path.endswith("/test_plan"):
            req = json.loads(body.decode("utf-8")) if body else {}
            tests = req.get("tests", {})
            files = [f["path"] for f in tests.get("files", [])] + [s["value"] for s in tests.get("selectors", [])]
            target = ""
            if os.path.exists(TARGET_OVERRIDE_FILE):
                target = open(TARGET_OVERRIDE_FILE).read().strip()
            selected = [{"format": "file", "path": target}] if target else [{"format": "file", "path": p} for p in files]
            resp = {
                "experiment": "plugin-demo",
                "tasks": {"0": {"node_number": 0, "tests": selected}},
            }
            self.wfile.write(json.dumps(resp).encode("utf-8"))
        else:
            self.wfile.write(b"{}")

server = HTTPServer(("127.0.0.1", 0), Handler)
with open(PORT_FILE, "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY

TARGET_FILE="$TMP_DIR/target.txt"
python3 "$TMP_DIR/mock_server.py" "$PORT_FILE" "$TARGET_FILE" &
MOCK_PID=$!
for _ in $(seq 1 50); do
  [[ -s "$PORT_FILE" ]] && break
  sleep 0.1
done
MOCK_URL="http://127.0.0.1:$(cat "$PORT_FILE")"
echo "Mock Test Engine server listening at $MOCK_URL"

should_run() {
  local name="$1"
  [[ -z "$FILTER_RUNNERS" ]] && return 0
  [[ ",$FILTER_RUNNERS," == *",$name,"* ]]
}

run_case() {
  local runner="$1" workdir="$2" target="$3"
  shift 3
  local extra_args=("$@")

  should_run "$runner" || return 0
  printf '%s' "$target" > "$TARGET_FILE"

  for variant in go bash; do
    local load_flag
    if [[ "$variant" == "go" ]]; then
      load_flag="$REPO_ROOT/plugins//go/$runner"
    else
      load_flag="$REPO_ROOT/plugins/bash/$runner"
    fi
    local res_dir="$TMP_DIR/results-$runner-$variant"
    mkdir -p "$res_dir"

    local resolved_args=()
    for a in "${extra_args[@]}"; do
      a="${a//__RES_DIR__/$res_dir}"
      resolved_args+=("$a")
    done

    printf '==> %-10s [%-4s] (%s)\n' "$runner" "$variant" "$load_flag"
    (
      cd "$workdir"
      env -u BUILDKITE_ANALYTICS_TOKEN -u BUILDKITE_TEST_ENGINE_API_ACCESS_TOKEN \
        CI=true \
        BUILDKITE_ORGANIZATION_SLUG=demo \
        BUILDKITE_PARALLEL_JOB=0 \
        BUILDKITE_TEST_ENGINE_BASE_URL="$MOCK_URL" \
        BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false \
        "$TMP_DIR/bktest" run \
          --suite-slug demo \
          --access-token demo-token \
          --no-oidc \
          --upload-results=false \
          --parallelism 1 \
          --plan-identifier "demo-$runner-$variant" \
          --plugin "$load_flag" \
          "${resolved_args[@]}" \
          >"$TMP_DIR/out-$runner-$variant.log" 2>&1
    )
    echo "    PASS ($runner / $variant)"
  done
}

if [[ -n "$EXAMPLES_DIR" ]]; then
  echo "Running against bktest-examples at $EXAMPLES_DIR"
  run_case rspec      "$EXAMPLES_DIR/rspec"      "" --test-command "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/rspec.json"
  run_case cucumber   "$EXAMPLES_DIR/cucumber"   "" --test-command "bundle exec cucumber --format progress --format json --out {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/cucumber.json"
  run_case jest       "$EXAMPLES_DIR/jest"       "" --test-command "npx jest --json --outputFile {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/jest.json"
  run_case vitest     "$EXAMPLES_DIR/vitest-4"   "" --test-command "npx vitest run --reporter=json --outputFile {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/vitest.json"
  run_case playwright "$EXAMPLES_DIR/playwright" "" --test-command "npx playwright test {{testExamples}}" --result-path "tmp/playwright-results.json"
  run_case cypress    "$EXAMPLES_DIR/cypress"    "" --test-command "npx cypress run --spec {{testExamples}}"
  run_case pytest     "$EXAMPLES_DIR/pytest"     "" --test-command "pytest {{testExamples}} --json={{resultPath}}" --result-path "__RES_DIR__/pytest.json"
  run_case gotest     "$EXAMPLES_DIR/go"         "" --test-command "go test -json {{packages}}"
  run_case custom     "$EXAMPLES_DIR/mocha"      "" --test-command "npx mocha {{testExamples}}" --test-file-pattern "src/**/*.test.js"
else
  TD="$REPO_ROOT/internal/runner/testdata"
  echo "Running against repository testdata suites at $TD"
  run_case rspec      "$TD/rspec"      "spec/spells/expelliarmus_spec.rb"     --test-command "rspec --format progress --format json --out {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/rspec.json"
  run_case jest       "$TD/jest"       "spells/expelliarmus.spec.js"          --test-command "npx jest --json --outputFile {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/jest.json"
  run_case vitest     "$TD/vitest"     "spells/expelliarmus.spec.js"          --test-command "npx vitest run --reporter=json --outputFile {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/vitest.json"
  run_case cypress    "$TD/cypress"    "cypress/e2e/passing_spec.cy.js"       --test-command "npx cypress run --spec {{testExamples}}"
  run_case playwright "$TD/playwright" "tests/example.spec.js"                --test-command "npx playwright test {{testExamples}}" --result-path "test-results/results.json"
  run_case pytest     "$TD/pytest"     "spells/test_expelliarmus.py"          --test-command "pytest {{testExamples}} --json={{resultPath}}" --result-path "__RES_DIR__/pytest.json"
  run_case gotest     "$TD/go"         "example.com/hello"                    --test-command "go test -json {{packages}}"
  run_case cucumber   "$TD/cucumber"   "features/spells/expelliarmus.feature" --test-command "cucumber --format progress --format json --out {{resultPath}} {{testExamples}}" --result-path "__RES_DIR__/cucumber.json"
  run_case custom     "$TD/custom"     "tests/happy_test.bats"                --test-command "bats --formatter junit --output __RES_DIR__ {{testExamples}}" --test-file-pattern "tests/*_test.bats" --result-path "__RES_DIR__/report.xml"
fi

echo "All selected plugin examples passed for both Go and Bash variants."
