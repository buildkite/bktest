package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/buildkite/bktest/v3/internal/pluginhost"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
)

// TestPluginParity runs the same scenarios against each built-in runner and
// its Bash and Go runner plugins in plugins/, with the real test frameworks
// in testdata, and requires the same observable results: features, discovered
// targets, examples, commands, and each attempt's error and RunResult.
func TestPluginParity(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the Bash runner plugins need jq")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	parityRepoRoot = root
	for _, sc := range parityScenarios() {
		t.Run(sc.runner+"/"+sc.name, func(t *testing.T) {
			for _, tool := range sc.requires {
				if _, err := exec.LookPath(tool); err != nil {
					t.Skipf("%s is not installed", tool)
				}
			}
			changeCwd(t, filepath.Join("testdata", sc.dir))
			want := sc.snapshot(t, "built-in")
			if testing.Verbose() {
				data, _ := json.MarshalIndent(want, "", "  ")
				t.Logf("built-in runner:\n%s", data)
			}
			for _, variant := range []string{"bash", "go"} {
				got := sc.snapshot(t, variant)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s plugin differs from the built-in runner (-built-in +%s):\n%s", variant, variant, diff)
				}
			}
		})
	}
}

type parityScenario struct {
	runner   string
	name     string
	dir      string
	requires []string
	config   RunnerConfig
	// discover and examples check target and example discovery.
	discover bool
	examples []string
	// commands are checked with CommandNameAndArgs, first and retry.
	commands [][]plan.TestCase
	// cases are run, then their failures are retried up to retries times.
	cases   []plan.TestCase
	retries int
}

type paritySnapshot struct {
	Name           string
	ResultFormat   string
	ResultPathBase string
	LocationPrefix string
	Features       SupportedFeatures
	Targets        []string
	Examples       []plan.TestCase
	Commands       []string
	Attempts       []parityAttempt
}

type parityAttempt struct {
	Err          string
	Status       RunStatus
	Error        string
	ErroredFiles []string
	Tests        []string
	Retryable    bool
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var (
	parityRepoRoot string
	goPluginsOnce  sync.Once
	goPluginsDir   string
	goPluginsErr   error
)

// pluginManifest returns the manifest of the runner's Bash plugin, loaded
// from its subdirectory, or of its Go plugin built into a temporary directory.
func pluginManifest(t *testing.T, runner, variant string) pluginhost.Manifest {
	t.Helper()
	root := filepath.Join(parityRepoRoot, "plugins")
	if variant == "bash" {
		m, err := pluginhost.LoadManifest(filepath.Join(root, runner, "bash"))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	goPluginsOnce.Do(func() {
		goPluginsDir, goPluginsErr = os.MkdirTemp("", "bktest-go-plugins-*")
		if goPluginsErr != nil {
			return
		}
	})
	if goPluginsErr != nil {
		t.Fatal(goPluginsErr)
	}
	// Build each runner's binary on first use.
	bin := filepath.Join(goPluginsDir, "bktest-runner-"+runner)
	if _, err := os.Stat(bin); err != nil {
		cmd := exec.Command("go", "build", "-o", bin, "./plugins/"+runner+"/go")
		cmd.Dir = filepath.Dir(root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", runner, err, out)
		}
	}
	return pluginhost.Manifest{Runner: runner, ProtocolVersion: 1, Command: []string{bin}, Dir: filepath.Join(root, runner, "go")}
}

func (sc parityScenario) newRunner(t *testing.T, variant string) TestRunnerWithTargetDiscovery {
	t.Helper()
	if variant == "built-in" {
		return builtinRunner(t, sc.runner, sc.config)
	}
	p, err := NewPlugin(pluginManifest(t, sc.runner, variant), sc.config, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func (sc parityScenario) snapshot(t *testing.T, variant string) paritySnapshot {
	t.Helper()
	r := sc.newRunner(t, variant)
	snap := paritySnapshot{
		Name:           r.Name(),
		ResultFormat:   r.ResultFormat(),
		ResultPathBase: filepath.Base(r.ResultFilePath()),
		LocationPrefix: r.LocationPrefix(),
		Features:       r.SupportedFeatures(),
	}
	if sc.discover {
		targets, err := r.DiscoverTestTargets()
		if err != nil {
			t.Fatalf("%s: DiscoverTestTargets: %v", variant, err)
		}
		slices.Sort(targets)
		snap.Targets = targets
	}
	if sc.examples != nil {
		examples, err := r.(ExampleDiscoverer).GetExamples(sc.examples)
		if err != nil {
			t.Fatalf("%s: GetExamples: %v", variant, err)
		}
		snap.Examples = examples
	}
	for _, cases := range sc.commands {
		for _, retry := range []bool{false, true} {
			name, args, err := r.CommandNameAndArgs(cases, retry)
			if err != nil {
				// Plugins word their errors themselves.
				snap.Commands = append(snap.Commands, "error")
				continue
			}
			snap.Commands = append(snap.Commands, shellquote.Join(append([]string{name}, args...)...))
		}
	}
	if sc.cases == nil {
		return snap
	}

	result := NewRunResult(nil)
	cases := sc.cases
	for attempt := 0; attempt <= sc.retries; attempt++ {
		if path := r.ResultFilePath(); path != "" {
			_ = os.Remove(path)
		}
		err := r.Run(result, cases, attempt > 0)
		snap.Attempts = append(snap.Attempts, attemptSnapshot(result, err))
		cases = result.FailedTests()
		if len(cases) == 0 || !result.Retryable() {
			break
		}
		slices.SortFunc(cases, func(a, b plan.TestCase) int { return strings.Compare(testIdentifier(a), testIdentifier(b)) })
	}
	return snap
}

func attemptSnapshot(result *RunResult, err error) parityAttempt {
	a := parityAttempt{Status: result.Status(), Retryable: result.Retryable(), ErroredFiles: slices.Sorted(slices.Values(result.ErroredFiles()))}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			a.Err = fmt.Sprintf("exit code %d", exitErr.ExitCode())
		} else {
			a.Err = fmt.Sprintf("%T: %v", err, err)
		}
	}
	if result.error != nil {
		a.Error = result.error.Error()
	}
	for key, test := range result.tests {
		// The Python collector gives each execution a random ID.
		test.Identifier = uuidPattern.ReplaceAllString(test.Identifier, "<uuid>")
		a.Tests = append(a.Tests, fmt.Sprintf("%s => %s x%d %+v", key, test.Status, test.ExecutionCount, test.TestCase))
	}
	slices.Sort(a.Tests)
	return a
}

func builtinRunner(t *testing.T, name string, rc RunnerConfig) TestRunnerWithTargetDiscovery {
	t.Helper()
	switch name {
	case "rspec":
		return NewRspec(rc)
	case "jest":
		return NewJest(rc)
	case "vitest":
		return NewVitest(rc)
	case "cypress":
		return NewCypress(rc)
	case "playwright":
		return NewPlaywright(rc)
	case "pytest":
		return NewPytest(rc)
	case "gotest":
		return NewGoTest(rc)
	case "cucumber":
		return NewCucumber(rc)
	}
	t.Fatalf("unknown runner %q", name)
	return nil
}

func parityScenarios() []parityScenario {
	tc := func(paths ...string) []plan.TestCase { return testCasesFromPaths(paths) }
	return []parityScenario{
		// RSpec
		{runner: "rspec", name: "discover, examples and commands", dir: "rspec", requires: []string{"rspec"},
			config:   RunnerConfig{TestCommand: "rspec", ResultPath: "tmp/rspec.json"},
			discover: true,
			examples: []string{"spec/specs_with_shared_examples_spec.rb", "spec/failure_spec.rb"},
			commands: [][]plan.TestCase{tc("spec/a_spec.rb", "spec/b c_spec.rb[1:2]")},
		},
		{runner: "rspec", name: "custom placeholders", dir: "rspec",
			config:   RunnerConfig{TestCommand: `bin/rspec --format json --out {{resultPath}} {{testExamples}} --tag "a b" --seed 1`, RetryTestCommand: "bin/rspec --only-failures", ResultPath: "out.json"},
			commands: [][]plan.TestCase{tc("spec/a_spec.rb")},
		},
		{runner: "rspec", name: "unterminated quote", dir: "rspec",
			config:   RunnerConfig{TestCommand: "rspec --tag 'oops", ResultPath: "out.json"},
			commands: [][]plan.TestCase{tc("spec/a_spec.rb")},
		},
		{runner: "rspec", name: "failures are retried by example ID", dir: "rspec", requires: []string{"rspec"},
			config:  RunnerConfig{TestCommand: "rspec --format json --out {{resultPath}}", ResultPath: "tmp/rspec.json"},
			cases:   tc("spec/failure_spec.rb", "spec/spells/expelliarmus_spec.rb", "spec/skipped_spec.rb", "spec/specs_with_shared_examples_spec.rb"),
			retries: 1,
		},
		{runner: "rspec", name: "error outside examples", dir: "rspec", requires: []string{"rspec"},
			config: RunnerConfig{TestCommand: "rspec --format json --out {{resultPath}}", ResultPath: "tmp/rspec.json"},
			cases:  tc("spec/bad_syntax_spec.rb", "spec/failure_spec.rb"), retries: 1,
		},
		{runner: "rspec", name: "no report", dir: "rspec", requires: []string{"rspec"},
			config: RunnerConfig{TestCommand: "rspec", ResultPath: "tmp/rspec.json"},
			cases:  tc("spec/failure_spec.rb"),
		},
		{runner: "rspec", name: "command fails", dir: "rspec", requires: []string{"rspec"},
			config: RunnerConfig{TestCommand: "rspec --invalid-option", ResultPath: "tmp/rspec.json"},
			cases:  tc("spec/failure_spec.rb"),
		},
		{runner: "rspec", name: "command is signaled", dir: "rspec",
			config: RunnerConfig{TestCommand: "../segv.sh", ResultPath: "tmp/rspec.json"},
			cases:  tc("spec/failure_spec.rb"),
		},

		// Cucumber
		{runner: "cucumber", name: "discover, examples and commands", dir: "cucumber", requires: []string{"cucumber"},
			config:   RunnerConfig{TestCommand: "cucumber", ResultPath: "tmp/cucumber.json"},
			discover: true,
			examples: []string{"features/simple_scenarios.feature", "features/failure.feature"},
			commands: [][]plan.TestCase{tc("features/a.feature", "features/b.feature:12")},
		},
		{runner: "cucumber", name: "scenario statuses and retries", dir: "cucumber", requires: []string{"cucumber"},
			config: RunnerConfig{TestCommand: "cucumber --format json --out {{resultPath}} {{testExamples}}", ResultPath: "tmp/cucumber.json"},
			cases:  tc("features/simple_scenarios.feature", "features/failure.feature", "features/spells/expelliarmus.feature", "features/another_feature.feature"), retries: 1,
		},
		{runner: "cucumber", name: "no report", dir: "cucumber", requires: []string{"cucumber"},
			config: RunnerConfig{TestCommand: "cucumber", ResultPath: "tmp/missing.json"},
			cases:  tc("features/failure.feature"),
		},

		// Jest
		{runner: "jest", name: "discover and commands", dir: "jest",
			config:   RunnerConfig{ResultPath: "tmp/jest.json"},
			discover: true,
			commands: [][]plan.TestCase{{{Path: "b.spec.js"}, {Path: "a.spec.js", Scope: "math (+)", Name: "adds 1 + 2 [*]"}, {Path: "b.spec.js", Name: "no scope"}}},
		},
		{runner: "jest", name: "missing retry sentinel", dir: "jest",
			config:   RunnerConfig{ResultPath: "tmp/jest.json", TestCommand: "jest {{testExamples}}", RetryTestCommand: "jest --outputFile {{resultPath}}"},
			commands: [][]plan.TestCase{tc("a.spec.js")},
		},
		{runner: "jest", name: "failures, runtime errors and retries", dir: "jest", requires: []string{"npx"},
			config: RunnerConfig{TestCommand: "npx jest --json --outputFile {{resultPath}}", RetryTestCommand: "npx jest --testNamePattern '{{testNamePattern}}' --json --outputFile {{resultPath}}", ResultPath: "tmp/jest.json"},
			cases:  tc("failure.spec.js", "runtimeError.spec.js", "skipped.spec.js", "spells/expelliarmus.spec.js"), retries: 1,
		},

		// Vitest
		{runner: "vitest", name: "discover and commands", dir: "vitest",
			config:   RunnerConfig{ResultPath: "vitest.json"},
			discover: true,
			commands: [][]plan.TestCase{{{Path: "b.spec.js"}, {Path: "a.spec.js", Scope: "outer inner", Name: "adds (1)"}}},
		},
		{runner: "vitest", name: "failures and retries with names from the report", dir: "vitest", requires: []string{"npx"},
			config: RunnerConfig{TestCommand: "npx vitest run {{testExamples}} --reporter=json --outputFile {{resultPath}}", RetryTestCommand: "npx vitest run --testNamePattern '{{testNamePattern}}' --reporter=json --outputFile {{resultPath}}", ResultPath: "vitest.json"},
			cases:  tc("failure.spec.js", "skipped.spec.js", "spells/expelliarmus.spec.js"), retries: 1,
		},
		{runner: "vitest", name: "exit 0 with failures from an earlier attempt", dir: "vitest",
			config: RunnerConfig{TestCommand: "npx vitest run {{testExamples}} --reporter=json --outputFile {{resultPath}}", RetryTestCommand: "true {{resultPath}} {{testNamePattern}}", ResultPath: "vitest.json"},
			cases:  tc("failure.spec.js"), retries: 1, requires: []string{"npx"},
		},

		// Playwright
		{runner: "playwright", name: "discover, examples and commands", dir: "playwright", requires: []string{"npx"},
			config:   RunnerConfig{TestCommand: "npx playwright test", ResultPath: "test-results/results.json"},
			discover: true,
			examples: []string{"tests/example.spec.js", "tests/failed.spec.js"},
			commands: [][]plan.TestCase{tc("tests/example.spec.js", "tests/failed.spec.js:12")},
		},
		{runner: "playwright", name: "projects, failures, skips and retries", dir: "playwright", requires: []string{"npx"},
			config: RunnerConfig{TestCommand: "npx playwright test", ResultPath: "test-results/results.json"},
			cases:  tc("tests/example.spec.js", "tests/failed.spec.js", "tests/skipped.spec.js"), retries: 1,
		},
		{runner: "playwright", name: "a file that fails to load reruns the others", dir: "playwright", requires: []string{"npx"},
			config: RunnerConfig{TestCommand: "npx playwright test", ResultPath: "test-results/results.json"},
			cases:  tc("./tests/example.spec.js", "./tests/error.spec.js"),
		},
		{runner: "playwright", name: "an error outside the given files", dir: "playwright", requires: []string{"npx"},
			config: RunnerConfig{TestCommand: "npx playwright test", ResultPath: "test-results/results.json"},
			cases:  tc("./tests/example.spec.js", "error"),
		},

		// pytest
		{runner: "pytest", name: "collector JSON: discover, examples, commands, retries", dir: "pytest", requires: []string{"pytest"},
			config:   RunnerConfig{ResultPath: "tmp/pytest.json"},
			discover: true,
			examples: []string{"test_sample.py", "failed_test.py"},
			commands: [][]plan.TestCase{tc("test_sample.py", "tests/test a.py::TestX::test_y[1]")},
			cases:    tc("test_sample.py", "failed_test.py", "spells/test_expelliarmus.py"), retries: 1,
		},
		{runner: "pytest", name: "JUnit XML: examples and retries", dir: "pytest", requires: []string{"pytest"},
			config:   RunnerConfig{TestCommand: "pytest {{testExamples}} --junit-xml={{resultPath}}", ResultPath: "tmp/pytest.xml"},
			examples: []string{"test_sample.py", "failed_test.py", "spells/test_expelliarmus.py"},
			cases:    tc("test_sample.py", "failed_test.py", "spells/test_expelliarmus.py"), retries: 1,
		},
		{runner: "pytest", name: "collection error", dir: "pytest_collection_error", requires: []string{"pytest"},
			config: RunnerConfig{ResultPath: "tmp/pytest.json"},
			cases:  tc("test_broken_import.py"),
		},
		{runner: "pytest", name: "collection error with JUnit XML", dir: "pytest_collection_error", requires: []string{"pytest"},
			config: RunnerConfig{TestCommand: "pytest {{testExamples}} --junit-xml={{resultPath}}", ResultPath: "tmp/pytest.xml"},
			cases:  tc("test_broken_import.py"),
		},

		// go test
		{runner: "gotest", name: "discover and commands", dir: "go", requires: []string{"go"},
			config:   RunnerConfig{ResultPath: "tmp/go.json"},
			discover: true,
			commands: [][]plan.TestCase{{{Path: "example.com/hello"}, {Format: plan.TestCaseFormatSelector, Value: "example.com/hello/bad"}, {Path: "example.com/hello"}}},
		},
		{runner: "gotest", name: "gotestsum JSON: failures, build failures and TestMain", dir: "go", requires: []string{"gotestsum"},
			config: RunnerConfig{ResultPath: "tmp/go.jsonl"},
			cases:  []plan.TestCase{{Path: "example.com/hello"}, {Path: "example.com/hello/bad"}, {Path: "example.com/hello/testmain"}}, retries: 1,
		},
		{runner: "gotest", name: "go test -json captured from stdout", dir: "go", requires: []string{"go"},
			config: RunnerConfig{TestCommand: "go test -json {{packages}}", ResultPath: "tmp/go-stdout.jsonl"},
			cases:  []plan.TestCase{{Path: "example.com/hello"}, {Path: "example.com/hello/broken"}},
		},
		{runner: "gotest", name: "gotestsum JUnit XML", dir: "go", requires: []string{"gotestsum"},
			config: RunnerConfig{TestCommand: "gotestsum --junitfile {{resultPath}} {{packages}}", ResultPath: "tmp/go.xml"},
			cases:  []plan.TestCase{{Path: "example.com/hello"}, {Path: "example.com/hello/broken"}},
		},

		// Cypress
		{runner: "cypress", name: "discover and commands", dir: "cypress",
			discover: true,
			commands: [][]plan.TestCase{tc("cypress/e2e/a.cy.js", "cypress/e2e/b c.cy.js")},
		},
		{runner: "cypress", name: "exit status only", dir: "cypress",
			config: RunnerConfig{TestCommand: "sh -c 'echo \"$@\"; exit 3' sh --spec {{testExamples}}"},
			cases:  tc("cypress/e2e/failing_spec.cy.js", "cypress/e2e/passing_spec.cy.js"),
		},
	}
}
