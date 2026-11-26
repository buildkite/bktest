//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeRSpec stands in for RSpec, so the tests need no test framework. It is
// run as `fake-rspec <report> <examples...>` and logs its arguments to
// calls.log. It answers a dry run with one example per file. Otherwise it
// writes a report where the example in flaky_spec.rb fails the first time,
// and the examples in slow_spec.rb wait for SIGTERM, then fail.
const fakeRSpec = `#!/usr/bin/env bash
out=$1
shift
echo "$*" >>calls.log

example() {
  printf '{"id":"%s","description":"works","full_description":"%s works","status":"%s","file_path":"%s","line_number":1}' "$1" "$1" "$2" "${1%%[*}"
}
report() {
  local file=$1 failures=$2 count=$3
  shift 3
  local IFS=,
  printf '{"examples":[%s],"summary":{"example_count":%d,"failure_count":%d,"pending_count":0,"errors_outside_of_examples_count":0}}' "$*" "$count" "$failures" >"$file"
}

args=("$@")
for i in "${!args[@]}"; do
  if [[ ${args[i]} == --dry-run ]]; then
    for j in "${!args[@]}"; do
      [[ ${args[j]} == --out ]] && dry=${args[j + 1]}
    done
    examples=()
    for arg in "${args[@]:0:i}"; do
      examples+=("$(example "./$arg[1:1]" passed)")
    done
    report "$dry" 0 "${#examples[@]}" "${examples[@]}"
    exit 0
  fi
done

examples=()
failures=0
for arg in "$@"; do
  status=passed
  case $arg in
  *flaky_spec.rb*)
    if [[ ! -e flaky-attempted ]]; then
      touch flaky-attempted
      status=failed
    fi
    ;;
  *slow_spec.rb*)
    # bktest forwards the signal the process group already got.
    trap 'trap "" TERM; echo "test process: got TERM"' TERM
    echo "test process: started"
    touch started
    sleep 30 &
    wait
    status=failed
    ;;
  esac
  [[ $status == failed ]] && failures=$((failures + 1))
  examples+=("$(example "$arg" "$status")")
done
report "$out" "$failures" "${#examples[@]}" "${examples[@]}"
[[ $failures -eq 0 ]]
`

// fakeTestEngine answers the requests `bktest run` makes: it has no cached
// plan, asks bktest to split every file into examples, puts the tests it is
// sent in one task, and accepts the run's metadata. It records the plans
// bktest asks for.
type fakeTestEngine struct {
	*httptest.Server
	mu    sync.Mutex
	plans []map[string]any
}

func newFakeTestEngine(t *testing.T) *fakeTestEngine {
	t.Helper()
	engine := &fakeTestEngine{}
	engine.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method == http.MethodPost {
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/test_plan/filter_tests"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tests": body["files"]})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/test_plan"):
			engine.mu.Lock()
			engine.plans = append(engine.plans, body)
			engine.mu.Unlock()
			tests := []any{}
			requested, _ := body["tests"].(map[string]any)
			if examples, ok := requested["examples"].([]any); ok {
				for _, example := range examples {
					example.(map[string]any)["format"] = "example"
					tests = append(tests, example)
				}
			}
			if selectors, ok := requested["selectors"].([]any); ok {
				for _, selector := range selectors {
					value := selector.(map[string]any)["value"]
					tests = append(tests, map[string]any{"format": "selector", "value": value, "path": value})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"identifier":  body["identifier"],
				"parallelism": 1,
				"tasks":       map[string]any{"0": map[string]any{"node_number": 0, "tests": tests}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/test_plan_metadata"):
			_, _ = io.WriteString(w, "{}")
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not found"}`)
		}
	}))
	t.Cleanup(engine.Close)
	return engine
}

// runnerPluginFixture is a release build of bktest and the rspec and cypress
// runner plugins, installed outside the repository the way a user would
// have them.
type runnerPluginFixture struct {
	bktest  string
	plugins string
}

func newRunnerPluginFixture(t *testing.T) runnerPluginFixture {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := runnerPluginFixture{bktest: filepath.Join(dir, "bin", "bktest"), plugins: filepath.Join(dir, "plugins")}

	build := func(output, pkg string) {
		t.Helper()
		// The flags of a release build, which has no access to the repository.
		cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", output, pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	copyFile := func(from, to string) {
		t.Helper()
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	build(fixture.bktest, "./cmd/bktest")
	copyFile(filepath.Join(root, "plugins", "lib", "runner-plugin.bash"), filepath.Join(fixture.plugins, "lib", "runner-plugin.bash"))
	for _, runner := range []string{"rspec", "cypress"} {
		for _, file := range []string{"bash/plugin.json", "bash/plugin.bash", "go/plugin.json"} {
			copyFile(filepath.Join(root, "plugins", runner, file), filepath.Join(fixture.plugins, runner, file))
		}
		build(filepath.Join(fixture.plugins, runner, "go", "bktest-runner-"+runner), "./plugins/"+runner+"/go")
	}
	return fixture
}

// variants are the ways to run a runner: built in, or as an external plugin
// loaded from a subdirectory of the plugins directory.
func (f runnerPluginFixture) variants(t *testing.T, runner string) map[string][]string {
	t.Helper()
	variants := map[string][]string{
		"built-in": {"--test-runner", runner},
		"go":       {"--runner-plugin", filepath.Join(f.plugins, runner, "go")},
	}
	if _, err := exec.LookPath("jq"); err == nil {
		variants["bash"] = []string{"--runner-plugin", filepath.Join(f.plugins, runner, "bash")}
	} else {
		t.Log("jq is not installed: not testing the Bash plugins")
	}
	return variants
}

// command returns `bktest run` in dir, in its own process group, against
// the fake Test Engine.
func (f runnerPluginFixture) command(engine *fakeTestEngine, dir string, env []string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.Command(f.bktest, append([]string{"run"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"BUILDKITE_TEST_ENGINE_BASE_URL=" + engine.URL,
		"BUILDKITE_TEST_ENGINE_API_ACCESS_TOKEN=token",
		"BUILDKITE_ORGANIZATION_SLUG=org",
		"BUILDKITE_TEST_ENGINE_SUITE_SLUG=suite",
		"BUILDKITE_BUILD_ID=build",
		"BUILDKITE_STEP_ID=step",
		"BUILDKITE_PARALLEL_JOB=0",
		"BUILDKITE_PARALLEL_JOB_COUNT=1",
		"BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false",
	}, env...)
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, output
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunnerPluginsEndToEnd runs a release build of bktest with a runner
// built in, as an external Go plugin and as a Bash plugin, and requires the
// same outcome of each.
func TestRunnerPluginsEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds bktest and the runner plugins")
	}
	fixture := newRunnerPluginFixture(t)

	t.Run("plans, splits by example and retries the same tests", func(t *testing.T) {
		calls := map[string]string{}
		plans := map[string][]map[string]any{}
		for variant, args := range fixture.variants(t, "rspec") {
			engine := newFakeTestEngine(t)
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"fake-rspec": fakeRSpec, "spec/a_spec.rb": "", "spec/flaky_spec.rb": "", "spec/nested/b_spec.rb": ""})
			cmd, output := fixture.command(engine, dir, []string{
				"BUILDKITE_TEST_ENGINE_TEST_CMD=./fake-rspec {{resultPath}} {{testExamples}}",
				"BUILDKITE_TEST_ENGINE_RESULT_PATH=tmp-rspec.json",
				"BUILDKITE_TEST_ENGINE_SPLIT_BY_EXAMPLE=true",
				"BUILDKITE_TEST_ENGINE_RETRY_COUNT=1",
			}, args...)
			if err := cmd.Run(); err != nil {
				t.Errorf("%s: bktest run: %v\n%s", variant, err, output)
				continue
			}
			for _, want := range []string{"Attempt 1 of 1 to retry failing tests", "Buildkite Test Engine Report"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("%s: output has no %q:\n%s", variant, want, output)
				}
			}
			if external := strings.Contains(output.String(), "using the RSpec runner plugin at "+fixture.plugins); external != (variant != "built-in") {
				t.Errorf("%s: output says an external plugin is used = %v:\n%s", variant, external, output)
			}
			data, err := os.ReadFile(filepath.Join(dir, "calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			// The dry run's report is a temporary file.
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if before, _, found := strings.Cut(line, " --out "); found {
					lines[i] = before + " --out <file> --format progress"
				}
			}
			calls[variant] = strings.Join(lines, "\n")
			plans[variant] = engine.plans
		}

		wantCalls := strings.Join([]string{
			"spec/a_spec.rb spec/flaky_spec.rb spec/nested/b_spec.rb --dry-run --format json --out <file> --format progress",
			"spec/a_spec.rb[1:1] spec/flaky_spec.rb[1:1] spec/nested/b_spec.rb[1:1]",
			"spec/flaky_spec.rb[1:1]",
			"",
		}, "\n")
		for variant := range calls {
			if diff := cmp.Diff(wantCalls, calls[variant]); diff != "" {
				t.Errorf("%s ran different test commands (-want +got):\n%s", variant, diff)
			}
			if diff := cmp.Diff(plans["built-in"], plans[variant]); diff != "" {
				t.Errorf("%s asked Test Engine for a different plan (-built-in +%s):\n%s", variant, variant, diff)
			}
		}
		if len(plans["built-in"]) != 1 || plans["built-in"][0]["runner"] != "rspec" {
			t.Errorf("plan requests = %v, want one for the rspec runner", plans["built-in"])
		}
	})

	// A CI agent cancels a job by signalling the whole process group. bktest
	// must still report the tests, and exit like the test command did.
	t.Run("survives SIGTERM to the process group", func(t *testing.T) {
		for variant, args := range fixture.variants(t, "rspec") {
			engine := newFakeTestEngine(t)
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"fake-rspec": fakeRSpec, "spec/slow_spec.rb": ""})
			cmd, output := fixture.command(engine, dir, []string{
				"BUILDKITE_TEST_ENGINE_TEST_CMD=./fake-rspec {{resultPath}} {{testExamples}}",
				"BUILDKITE_TEST_ENGINE_RESULT_PATH=tmp-rspec.json",
				"BUILDKITE_TEST_ENGINE_RETRY_COUNT=0",
			}, args...)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(30 * time.Second)
			for !fileExists(filepath.Join(dir, "started")) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err := cmd.Wait()
			if code := exitCode(err); code != 1 {
				t.Errorf("%s: bktest exited with %d (%v), want 1:\n%s", variant, code, err, output)
			}
			for _, want := range []string{"test process: got TERM", "Buildkite Test Engine Report", "spec/slow_spec.rb"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("%s: output has no %q:\n%s", variant, want, output)
				}
			}
		}
	})

	// A built-in runner is chosen by name alone: a directory that happens to
	// be named after it, as every Cypress project has, isn't a plugin.
	t.Run("a built-in runner ignores a directory with its name", func(t *testing.T) {
		calls := map[string]string{}
		for variant, args := range fixture.variants(t, "cypress") {
			engine := newFakeTestEngine(t)
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{
				"cypress/e2e/a.cy.js":   "",
				"cypress/e2e/b.cy.js":   "",
				"cypress/plugin.json":   `{"runner": "cypress", "protocol_version": 1, "command": ["false"]}`,
				"fake-cypress":          "#!/bin/sh\necho \"$@\" >>calls.log\n",
				"node_modules/x.cy.js":  "",
				"plugins/cypress/index": "",
			})
			cmd, output := fixture.command(engine, dir, []string{"BUILDKITE_TEST_ENGINE_TEST_CMD=./fake-cypress --spec {{testExamples}}"}, args...)
			if err := cmd.Run(); err != nil {
				t.Errorf("%s: bktest run: %v\n%s", variant, err, output)
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, "calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			calls[variant] = string(data)
		}
		for variant, got := range calls {
			if want := "--spec cypress/e2e/a.cy.js,cypress/e2e/b.cy.js\n"; got != want {
				t.Errorf("%s ran %q, want %q", variant, got, want)
			}
		}
	})

	t.Run("rejects a plugin for another runner", func(t *testing.T) {
		engine := newFakeTestEngine(t)
		cmd, output := fixture.command(engine, t.TempDir(), nil, "--test-runner", "jest", "--result-path", "jest.json", "--runner-plugin", filepath.Join(fixture.plugins, "rspec", "go"))
		err := cmd.Run()
		want := fmt.Sprintf("runner plugin %q is for the test runner \"rspec\", not \"jest\"", filepath.Join(fixture.plugins, "rspec", "go"))
		if exitCode(err) != 16 || !strings.Contains(output.String(), want) {
			t.Errorf("bktest exited with %v, want exit status 16 and %q:\n%s", err, want, output)
		}
	})

	t.Run("reports a plugin that isn't built", func(t *testing.T) {
		engine := newFakeTestEngine(t)
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"plugin/plugin.json": `{"runner": "rspec", "protocol_version": 1, "command": ["./missing"]}`})
		cmd, output := fixture.command(engine, dir, nil, "--result-path", "rspec.json", "--runner-plugin", "plugin")
		err := cmd.Run()
		if exitCode(err) != 16 || !strings.Contains(output.String(), "not found; build or install the plugin first") {
			t.Errorf("bktest exited with %v, want exit status 16 and a hint to build the plugin:\n%s", err, output)
		}
	})

	// bktest's own runners answer plugin operations too.
	t.Run("serves a built-in runner as a plugin", func(t *testing.T) {
		engine := newFakeTestEngine(t)
		dir := t.TempDir()
		manifest, _ := json.Marshal(map[string]any{"runner": "cypress", "protocol_version": 1, "command": []string{fixture.bktest, "tools", "runner-plugin", "cypress"}})
		writeFiles(t, dir, map[string]string{
			"cypress/e2e/a.cy.js": "",
			"self/plugin.json":    string(manifest),
			"fake-cypress":        "#!/bin/sh\necho \"$@\" >>calls.log\n",
		})
		cmd, output := fixture.command(engine, dir, []string{"BUILDKITE_TEST_ENGINE_TEST_CMD=./fake-cypress --spec {{testExamples}}"}, "--runner-plugin", "self/plugin.json")
		if err := cmd.Run(); err != nil {
			t.Fatalf("bktest run: %v\n%s", err, output)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		if want := "--spec cypress/e2e/a.cy.js\n"; string(data) != want {
			t.Errorf("ran %q, want %q", data, want)
		}
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
