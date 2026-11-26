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
	"github.com/buildkite/bktest/v3/internal/runners"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kballard/go-shellquote"
)

// parityVariants are the ways bktest can call a runner plugin: the built-in
// handler in this process, the same handler built as an external Go plugin,
// and the Bash plugin, which implements the API on its own. The first is the
// reference the others must match.
var parityVariants = []string{"built-in", "go", "bash"}

// TestPluginParity runs the same scenarios against each runner's variants,
// with the real test frameworks in testdata, and requires the same
// observable results: features, discovered targets, examples, commands,
// each attempt's error, messages and RunResult, and parsed pool reports.
func TestPluginParity(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the Bash runner plugins need jq")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	parityRepoRoot = root
	// To work on one variant: BKTEST_PARITY_VARIANTS=built-in,bash
	if variants := os.Getenv("BKTEST_PARITY_VARIANTS"); variants != "" {
		parityVariants = strings.Split(variants, ",")
	}
	for _, sc := range parityScenarios() {
		t.Run(sc.runner+"/"+sc.name, func(t *testing.T) {
			for _, tool := range sc.requires {
				if _, err := exec.LookPath(tool); err != nil {
					t.Skipf("%s is not installed", tool)
				}
			}
			for key, value := range sc.env {
				t.Setenv(key, value)
			}
			changeCwd(t, filepath.Join("testdata", sc.dir))
			want := sc.snapshot(t, parityVariants[0])
			if testing.Verbose() {
				data, _ := json.MarshalIndent(want, "", "  ")
				t.Logf("%s:\n%s", parityVariants[0], data)
			}
			for _, variant := range parityVariants[1:] {
				got := sc.snapshot(t, variant)
				if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("%s differs from %s (-%s +%s):\n%.4000s", variant, parityVariants[0], parityVariants[0], variant, diff)
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
	env      map[string]string
	config   RunnerConfig
	// discover and examples check target and example discovery.
	discover bool
	examples []string
	// commands are checked with CommandNameAndArgs, first and retry.
	commands [][]plan.TestCase
	// cases are run, then their failures are retried up to retries times.
	// keepReport leaves the report of a fixture in place before each attempt.
	cases      []plan.TestCase
	retries    int
	keepReport bool
	// reports are parsed as Test Scheduler pool reports, as is the report of
	// each attempt when the runner supports pools.
	reports []string
}

type paritySnapshot struct {
	// ConfigError is whether the runner rejected the configuration. Plugins
	// word their errors themselves.
	ConfigError    bool
	Name           string
	ResultFormat   string
	ResultPathBase string
	LocationPrefix string
	TestCommand    string
	RetryCommand   string
	FilePattern    string
	Features       SupportedFeatures
	Targets        []string
	Examples       []plan.TestCase
	Commands       []string
	Attempts       []parityAttempt
	Reports        []parityReport
}

type parityAttempt struct {
	Err          string
	Messages     []string
	Status       RunStatus
	Error        string
	ErroredFiles []string
	Tests        []string
	Retryable    bool
	Report       *parityReport
}

type parityReport struct {
	Format string
	Error  bool
	Parsed ParsedReport
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
	m, err := pluginhost.LoadManifest(filepath.Join(root, runner, variant))
	if err != nil {
		t.Fatal(err)
	}
	if variant == "bash" {
		return m
	}
	goPluginsOnce.Do(func() {
		goPluginsDir, goPluginsErr = os.MkdirTemp("", "bktest-go-plugins-*")
	})
	if goPluginsErr != nil {
		t.Fatal(goPluginsErr)
	}
	// Build each runner's binary on first use.
	bin := filepath.Join(goPluginsDir, "bktest-runner-"+runner)
	if _, err := os.Stat(bin); err != nil {
		cmd := exec.Command("go", "build", "-o", bin, "./plugins/"+runner+"/go")
		cmd.Dir = parityRepoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", runner, err, out)
		}
	}
	m.Command = []string{bin}
	return m
}

// parityRunner is what the scenarios need from a runner.
type parityRunner interface {
	TestRunnerWithTargetDiscovery
	// batchReportFormat is the format of the report at the result path for
	// Test Scheduler pools, or empty if the runner doesn't support them.
	batchReportFormat() string
	parseBatchReport(format string, data []byte) (ParsedReport, error)
	getExamples(files []string) ([]plan.TestCase, error)
}

type pluginParityRunner struct{ *PluginRunner }

func (p pluginParityRunner) batchReportFormat() string {
	if !p.supports("parse-batch-report") {
		return ""
	}
	return p.BatchReportFormat()
}

func (p pluginParityRunner) parseBatchReport(format string, data []byte) (ParsedReport, error) {
	return p.ParseBatchReport(format, data)
}

func (p pluginParityRunner) getExamples(files []string) ([]plan.TestCase, error) {
	return p.GetExamples(files)
}

func (sc parityScenario) newRunner(t *testing.T, variant string) (parityRunner, error) {
	t.Helper()
	var plugin pluginhost.Plugin
	switch variant {
	case "built-in":
		newHandler, ok := runners.Builtin[sc.runner]
		if !ok {
			t.Fatalf("no built-in runner %q", sc.runner)
		}
		plugin = pluginhost.InProcess{New: newHandler}
	default:
		plugin = pluginhost.External{Manifest: pluginManifest(t, sc.runner, variant)}
	}
	p, err := NewPluginRunner(plugin, sc.config)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = p.Close() })
	return pluginParityRunner{p}, nil
}

func (sc parityScenario) snapshot(t *testing.T, variant string) paritySnapshot {
	t.Helper()
	r, err := sc.newRunner(t, variant)
	if err != nil {
		t.Logf("%s: configuration rejected: %v", variant, err)
		return paritySnapshot{ConfigError: true}
	}
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
			t.Logf("%s: DiscoverTestTargets: %v", variant, err)
			targets = []string{"error"}
		}
		slices.Sort(targets)
		snap.Targets = targets
	}
	if sc.examples != nil {
		examples, err := r.getExamples(sc.examples)
		if err != nil {
			t.Logf("%s: GetExamples: %v", variant, err)
			examples = []plan.TestCase{{Name: "error"}}
		}
		snap.Examples = examples
	}
	for _, cases := range sc.commands {
		for _, retry := range []bool{false, true} {
			name, args, err := r.CommandNameAndArgs(cases, retry)
			if err != nil {
				// Plugins word their errors themselves.
				t.Logf("%s: CommandNameAndArgs: %v", variant, err)
				snap.Commands = append(snap.Commands, "error")
				continue
			}
			snap.Commands = append(snap.Commands, shellquote.Join(append([]string{name}, args...)...))
		}
	}
	for _, report := range sc.reports {
		snap.Reports = append(snap.Reports, parseParityReport(t, r, variant, []byte(report)))
	}
	if sc.cases == nil {
		return snap
	}

	result := NewRunResult(nil)
	cases := sc.cases
	for attempt := 0; attempt <= sc.retries; attempt++ {
		path := r.ResultFilePath()
		if path != "" && !sc.keepReport {
			_ = os.Remove(path)
			// Not every framework creates the report's directory.
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		messages, err := captureMessages(t, func() error { return r.Run(result, cases, attempt > 0) })
		a := attemptSnapshot(result, err)
		a.Messages = messages
		if data, readErr := os.ReadFile(path); readErr == nil && r.batchReportFormat() != "" {
			report := parseParityReport(t, r, variant, data)
			a.Report = &report
		}
		snap.Attempts = append(snap.Attempts, a)
		cases = result.FailedTests()
		if len(cases) == 0 || !result.Retryable() {
			break
		}
		slices.SortFunc(cases, func(a, b plan.TestCase) int { return strings.Compare(testIdentifier(a), testIdentifier(b)) })
	}
	return snap
}

func parseParityReport(t *testing.T, r parityRunner, variant string, data []byte) parityReport {
	t.Helper()
	report := parityReport{Format: r.batchReportFormat()}
	parsed, err := r.parseBatchReport(report.Format, data)
	if err != nil {
		t.Logf("%s: parsing the %s report: %v", variant, report.Format, err)
		report.Error = true
		return report
	}
	report.Parsed = parsed
	return report
}

// captureMessages runs fn and returns the lines bktest itself printed to
// stdout, which all start with its name, among the test command's output.
func captureMessages(t *testing.T, fn func() error) ([]string, error) {
	t.Helper()
	file, err := os.CreateTemp("", "parity-stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	stdout := os.Stdout
	os.Stdout = file
	runErr := fn()
	os.Stdout = stdout
	file.Close()

	output, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if testing.Verbose() {
		_, _ = os.Stdout.Write(output)
	}
	var messages []string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "Buildkite Test Engine Client") {
			messages = append(messages, line)
		}
	}
	return messages, runErr
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

func parityScenarios() []parityScenario {
	tc := func(paths ...string) []plan.TestCase { return testCasesFromPaths(paths) }
	many := make([]plan.TestCase, 5000)
	for i := range many {
		id := fmt.Sprintf("./spec/models/some/deeply/nested/thing_%04d_spec.rb[1:%d:3]", i, i%17)
		many[i] = plan.TestCase{Path: id, Identifier: id, Scope: fmt.Sprintf("Thing %d when something happens", i), Name: fmt.Sprintf("does the expected thing number %d", i)}
	}
	weird := []plan.TestCase{
		{Path: "a.spec.js", Scope: `it's "quoted" \ back`, Name: `$dollar ünï {brace} ^caret|pipe 日本語`},
		{Path: "dir with space/b.spec.js", Scope: "", Name: "tab\there"},
		{Path: "c.spec.js", Scope: "multi", Name: "line\nbreak"},
		{Path: "a.spec.js", Scope: "dup", Name: "dup"},
	}
	return []parityScenario{
		// RSpec
		{runner: "rspec", name: "discover, examples and commands", dir: "rspec", requires: []string{"rspec"},
			config:   RunnerConfig{TestCommand: "rspec", ResultPath: "tmp/rspec.json"},
			discover: true,
			examples: []string{"spec/specs_with_shared_examples_spec.rb", "spec/failure_spec.rb"},
			commands: [][]plan.TestCase{tc("spec/a_spec.rb", "spec/b c_spec.rb[1:2]")},
		},
		{runner: "rspec", name: "defaults", dir: "rspec",
			commands: [][]plan.TestCase{tc("spec/a_spec.rb")},
		},
		{runner: "rspec", name: "custom placeholders", dir: "rspec",
			config:   RunnerConfig{TestCommand: `bin/rspec --format json --out {{resultPath}} {{testExamples}} --tag "a b" --seed 1`, RetryTestCommand: "bin/rspec --only-failures", ResultPath: "out.json"},
			commands: [][]plan.TestCase{tc("spec/a_spec.rb")},
		},
		{runner: "rspec", name: "unterminated quote", dir: "rspec",
			config:   RunnerConfig{TestCommand: "rspec --tag 'oops", ResultPath: "out.json"},
			commands: [][]plan.TestCase{tc("spec/a_spec.rb")},
		},
		{runner: "rspec", name: "quoting and special characters", dir: "rspec",
			config:   RunnerConfig{TestCommand: `rspec --tag "it's" --pattern 'a b' -e \$x {{testExamples}} --out {{resultPath}}`, ResultPath: "tmp/r s.json"},
			commands: [][]plan.TestCase{weird},
		},
		{runner: "rspec", name: "empty arguments", dir: "rspec",
			config:   RunnerConfig{TestCommand: `env FOO='' BAR="a  b" rspec "" {{testExamples}}`, ResultPath: "r.json"},
			commands: [][]plan.TestCase{weird},
		},
		{runner: "rspec", name: "5000 examples", dir: "rspec",
			config:   RunnerConfig{TestCommand: "rspec --format json --out {{resultPath}} {{testExamples}}", ResultPath: "tmp/r.json"},
			commands: [][]plan.TestCase{many},
		},
		{runner: "rspec", name: "other formatters in the dry run", dir: "rspec", requires: []string{"rspec"},
			config:   RunnerConfig{TestCommand: "rspec --format documentation", ResultPath: "tmp/rspec.json"},
			examples: []string{"spec/spells/expelliarmus_spec.rb"},
		},
		{runner: "rspec", name: "dry run fails", dir: "rspec", requires: []string{"rspec"},
			config:   RunnerConfig{TestCommand: "rspec --nope", ResultPath: "tmp/rspec.json"},
			examples: []string{"spec/spells/expelliarmus_spec.rb"},
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
		{runner: "rspec", name: "command not found", dir: "rspec",
			config: RunnerConfig{TestCommand: "no-such-rspec-command", ResultPath: "tmp/rspec.json"},
			cases:  tc("spec/failure_spec.rb"),
		},
		{runner: "rspec", name: "pool reports", dir: "rspec",
			config: RunnerConfig{ResultPath: "tmp/rspec.json"},
			reports: []string{
				`{"examples":[
					{"id":"./spec/a_spec.rb[1:1]","description":"works","full_description":"A works","status":"passed","file_path":"./spec/a_spec.rb","line_number":3},
					{"id":"./spec/a_spec.rb[1:2]","description":"is shared","full_description":"A is shared","status":"failed","file_path":"./spec/shared.rb","line_number":9},
					{"id":"","description":"no id","full_description":"no id","status":"pending","file_path":"./spec/b_spec.rb","line_number":4},
					{"id":"./spec/c_spec.rb[x]","description":"odd id","full_description":"C odd id","status":"passed","file_path":"./spec/c_spec.rb","line_number":1}
				],"summary":{"example_count":4,"errors_outside_of_examples_count":1}}`,
				`{"examples":[],"summary":{"example_count":0}}`,
				`{"examples":[],"summary":{"example_count":1}}`,
				`{"examples":[]}`,
				`{"summary":{"example_count":0}}`,
				`{"examples":[{"id":"./spec/a_spec.rb[1:1]","status":"exploded"}],"summary":{"example_count":1}}`,
				`{"examples":[{"description":"nowhere","status":"passed"}],"summary":{"example_count":1}}`,
				`not json`,
			},
		},

		// Cucumber
		{runner: "cucumber", name: "discover, examples and commands", dir: "cucumber", requires: []string{"cucumber"},
			config:   RunnerConfig{TestCommand: "cucumber", ResultPath: "tmp/cucumber.json"},
			discover: true,
			examples: []string{"features/simple_scenarios.feature", "features/failure.feature"},
			commands: [][]plan.TestCase{tc("features/a.feature", "features/b.feature:12")},
		},
		{runner: "cucumber", name: "defaults and special characters", dir: "cucumber",
			commands: [][]plan.TestCase{weird},
		},
		{runner: "cucumber", name: "scenario statuses and retries", dir: "cucumber", requires: []string{"cucumber"},
			config: RunnerConfig{TestCommand: "cucumber --format json --out {{resultPath}} {{testExamples}}", ResultPath: "tmp/cucumber.json"},
			cases:  tc("features/simple_scenarios.feature", "features/failure.feature", "features/spells/expelliarmus.feature", "features/another_feature.feature"), retries: 1,
		},
		{runner: "cucumber", name: "line selectors", dir: "cucumber", requires: []string{"cucumber"},
			config: RunnerConfig{TestCommand: "cucumber --format json --out {{resultPath}} {{testExamples}}", ResultPath: "tmp/cucumber.json"},
			cases:  tc("features/failure.feature:3"), retries: 1,
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
		{runner: "jest", name: "special characters and 5000 retries", dir: "jest",
			config:   RunnerConfig{ResultPath: "tmp/jest.json"},
			commands: [][]plan.TestCase{weird, many},
		},
		{runner: "jest", name: "failures, runtime errors and retries", dir: "jest", requires: []string{"npx"},
			config: RunnerConfig{TestCommand: "npx jest --runInBand --no-cache --json --outputFile {{resultPath}}", RetryTestCommand: "npx jest --runInBand --no-cache --testNamePattern '{{testNamePattern}}' --json --outputFile {{resultPath}}", ResultPath: "tmp/jest.json"},
			cases:  tc("failure.spec.js", "runtimeError.spec.js", "skipped.spec.js", "spells/expelliarmus.spec.js"), retries: 1,
		},
		{runner: "jest", name: "pool reports", dir: "jest",
			config: RunnerConfig{ResultPath: "tmp/jest.json"},
			reports: []string{
				`{"numRuntimeErrorTestSuites":0,"testResults":[]}`,
				`{"testResults":[]}`,
				`{}`,
				`not json`,
			},
		},

		// Vitest
		{runner: "vitest", name: "discover and commands", dir: "vitest",
			config:   RunnerConfig{ResultPath: "vitest.json"},
			discover: true,
			commands: [][]plan.TestCase{{{Path: "b.spec.js"}, {Path: "a.spec.js", Scope: "outer inner", Name: "adds (1)"}}},
		},
		{runner: "vitest", name: "special characters and 5000 retries", dir: "vitest",
			config:   RunnerConfig{ResultPath: "tmp/v.json"},
			commands: [][]plan.TestCase{weird, many},
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
			commands: [][]plan.TestCase{tc("tests/example.spec.js", "tests/failed.spec.js:12"), weird},
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
		{runner: "playwright", name: "a failed setup project leaves tests not run", dir: "playwright", requires: []string{"npx"},
			env:    map[string]string{"PLAYWRIGHT_FAIL_SETUP": "1"},
			config: RunnerConfig{TestCommand: "npx playwright test", ResultPath: "test-results/results.json"},
			cases:  tc("tests/example.spec.js", "tests/skipped.spec.js"), retries: 1,
		},
		{runner: "playwright", name: "pool reports", dir: "playwright",
			config:  RunnerConfig{ResultPath: "test-results/results.json"},
			reports: []string{`{"suites":[],"errors":[]}`, `{}`, `not json`},
		},

		// pytest
		{runner: "pytest", name: "collector JSON: discover, examples, commands, retries", dir: "pytest", requires: []string{"pytest"},
			config:   RunnerConfig{ResultPath: "tmp/pytest.json"},
			discover: true,
			examples: []string{"test_sample.py", "failed_test.py"},
			commands: [][]plan.TestCase{tc("test_sample.py", "tests/test a.py::TestX::test_y[1]"), many},
			cases:    tc("test_sample.py", "failed_test.py", "spells/test_expelliarmus.py"), retries: 1,
		},
		{runner: "pytest", name: "JUnit XML: examples and retries", dir: "pytest", requires: []string{"pytest"},
			config:   RunnerConfig{TestCommand: "pytest {{testExamples}} --junit-xml={{resultPath}}", ResultPath: "tmp/pytest.xml"},
			examples: []string{"test_sample.py", "failed_test.py", "spells/test_expelliarmus.py"},
			cases:    tc("test_sample.py", "failed_test.py", "spells/test_expelliarmus.py"), retries: 1,
		},
		{runner: "pytest", name: "tag filter", dir: "pytest", requires: []string{"pytest"},
			config:   RunnerConfig{TagFilters: "team:frontend", ResultPath: "tmp/pytest.json"},
			examples: []string{"failed_test.py", "spells/test_expelliarmus.py", "test_sample.py"},
		},
		{runner: "pytest", name: "collection error", dir: "pytest_collection_error", requires: []string{"pytest"},
			config: RunnerConfig{ResultPath: "tmp/pytest.json"},
			cases:  tc("test_broken_import.py"),
		},
		{runner: "pytest", name: "collection error with JUnit XML", dir: "pytest_collection_error", requires: []string{"pytest"},
			config: RunnerConfig{TestCommand: "pytest {{testExamples}} --junit-xml={{resultPath}}", ResultPath: "tmp/pytest.xml"},
			cases:  tc("test_broken_import.py"),
		},
		{runner: "pytest", name: "pool reports", dir: "pytest", requires: []string{"pytest"},
			config:  RunnerConfig{ResultPath: "tmp/pytest.json"},
			reports: []string{`[]`, `{}`, `not json`},
		},
		{runner: "pytest", name: "JUnit XML pool reports", dir: "pytest", requires: []string{"pytest"},
			config:  RunnerConfig{TestCommand: "pytest {{testExamples}} --junit-xml={{resultPath}}", ResultPath: "tmp/pytest.xml"},
			reports: []string{`<testsuites></testsuites>`, `<testsuite></testsuite>`, `not xml`},
		},

		// go test
		{runner: "gotest", name: "discover and commands", dir: "go", requires: []string{"go"},
			config:   RunnerConfig{ResultPath: "tmp/go.json"},
			discover: true,
			commands: [][]plan.TestCase{{{Path: "example.com/hello"}, {Format: plan.TestCaseFormatSelector, Value: "example.com/hello/bad"}, {Path: "example.com/hello"}}, {{Format: plan.TestCaseFormatSelector, Value: "example.com/a b"}, {Path: "example.com/x"}, {Path: "example.com/x"}}},
		},
		{runner: "gotest", name: "gotestsum JSON: failures, build failures and TestMain", dir: "go", requires: []string{"gotestsum"},
			config: RunnerConfig{ResultPath: "tmp/go.jsonl"},
			cases:  []plan.TestCase{{Path: "example.com/hello"}, {Path: "example.com/hello/bad"}, {Path: "example.com/hello/testmain"}}, retries: 1,
		},
		{runner: "gotest", name: "selector test cases", dir: "go", requires: []string{"gotestsum"},
			config: RunnerConfig{ResultPath: "tmp/go.jsonl"},
			cases:  []plan.TestCase{{Format: plan.TestCaseFormatSelector, Value: "example.com/hello"}}, retries: 1,
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
			commands: [][]plan.TestCase{tc("cypress/e2e/a.cy.js", "cypress/e2e/b c.cy.js"), weird},
		},
		{runner: "cypress", name: "custom command without the placeholder", dir: "cypress",
			config:   RunnerConfig{TestCommand: "yarn cypress:run --browser 'chrome beta'", RetryTestCommand: "yarn retry {{testExamples}} --last"},
			commands: [][]plan.TestCase{tc("cypress/e2e/a.cy.js", "cypress/e2e/b.cy.js")},
		},
		{runner: "cypress", name: "exit status only", dir: "cypress",
			config: RunnerConfig{TestCommand: "sh -c 'echo \"$@\"; exit 3' sh --spec {{testExamples}}"},
			cases:  tc("cypress/e2e/failing_spec.cy.js", "cypress/e2e/passing_spec.cy.js"),
		},

		// Custom
		{runner: "custom", name: "discover, commands and no report", dir: "custom", requires: []string{"bats"},
			config:   RunnerConfig{TestCommand: "bats {{testExamples}}", TestFilePattern: "tests/**/*.bats"},
			discover: true,
			commands: [][]plan.TestCase{tc("tests/happy_test.bats", "tests/failed_test.bats"), weird},
			cases:    tc("tests/happy_test.bats", "tests/failed_test.bats"),
		},
		{runner: "custom", name: "the test command is split after the files are added", dir: "custom",
			config:   RunnerConfig{TestCommand: "bin/test --flag 'a b' {{testExamples}} --end {{testExamples}}", RetryTestCommand: "bin/retry", TestFilePattern: "tests/*.bats"},
			commands: [][]plan.TestCase{weird, many},
		},
		{runner: "custom", name: "JUnit XML report", dir: "custom", requires: []string{"bats"},
			config: RunnerConfig{TestCommand: "bats --report-formatter junit --output tmp {{testExamples}}", TestFilePattern: "tests/**/*.bats", ResultPath: "tmp/report.xml"},
			cases:  tc("tests/happy_test.bats", "tests/failed_test.bats"), retries: 1,
		},
		{runner: "custom", name: "Test Engine JSON report", dir: "custom",
			config: RunnerConfig{TestCommand: "sh -c 'exit 1' {{testExamples}}", TestFilePattern: "tests/**/*.bats", ResultPath: "test-result.json"},
			cases:  tc("tests/happy_test.bats"), keepReport: true,
		},
		{runner: "custom", name: "unreadable reports", dir: "custom",
			config: RunnerConfig{TestCommand: "true {{testExamples}}", TestFilePattern: "tests/**/*.bats", ResultPath: "tmp/missing.xml"},
			cases:  tc("tests/happy_test.bats"),
		},
		{runner: "custom", name: "selector list", dir: "custom",
			config:   RunnerConfig{TestCommand: "echo {{testExamples}}", SelectorListPath: "selectors.txt"},
			commands: [][]plan.TestCase{{{Format: plan.TestCaseFormatSelector, Value: "tests/happy_test.bats"}}},
		},
		{runner: "custom", name: "no test command", dir: "custom",
			config: RunnerConfig{TestFilePattern: "tests/**/*.bats"},
		},
		{runner: "custom", name: "no test file pattern", dir: "custom",
			config: RunnerConfig{TestCommand: "bats {{testExamples}}"},
		},
	}
}
