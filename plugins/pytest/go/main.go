// Command bktest-runner-pytest is the Go runner plugin for pytest.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin/junit"
	"github.com/kballard/go-shellquote"
	"golang.org/x/mod/semver"
)

const collectionErrorTag = "test.pytest_collection_error"

type pytest struct {
	cfg      runnerplugin.Config
	log      runnerplugin.Logger
	useJUnit bool
}

func main() { runnerplugin.Serve(&pytest{}) }

// Initialize picks the report format: the Buildkite test collector's JSON
// when its package is installed, otherwise JUnit XML. A --junit-xml or
// --json= flag in the test command overrides that.
func (p *pytest) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	p.cfg = params.Config
	p.log.Enabled = p.cfg.Debug
	collector := pythonPackageInstalled("buildkite_test_collector")
	p.useJUnit = !collector

	if collector {
		if p.cfg.TagFilters != "" {
			if err := checkCollectorVersion("1.2.0"); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				fmt.Fprintln(os.Stderr, "Please upgrade with: pip install --upgrade buildkite-test-collector")
				return runnerplugin.InitializeResult{}, err
			}
		}
		if p.cfg.TestCommand == "" {
			p.cfg.TestCommand = "pytest {{testExamples}} --json={{resultPath}}"
		}
		if p.cfg.ResultPath == "" {
			p.cfg.ResultPath = tempResultPath("pytest-results.json")
		}
	} else {
		if p.cfg.TagFilters != "" {
			fmt.Fprintln(os.Stderr, "Error: --tag-filters requires the 'buildkite-test-collector' package.")
			fmt.Fprintln(os.Stderr, "Please install it with: pip install buildkite-test-collector.")
			return runnerplugin.InitializeResult{}, fmt.Errorf("--tag-filters requires the 'buildkite-test-collector' package")
		}
		if p.cfg.TestCommand == "" {
			p.cfg.TestCommand = "pytest {{testExamples}} --junit-xml={{resultPath}}"
		}
		if p.cfg.ResultPath == "" {
			p.cfg.ResultPath = tempResultPath("pytest-results.xml")
		}
	}

	if strings.Contains(p.cfg.TestCommand, "--junit-xml") {
		p.useJUnit = true
	} else if strings.Contains(p.cfg.TestCommand, "--json=") {
		if !collector {
			fmt.Fprintln(os.Stderr, "Error: --json requires the 'buildkite-test-collector' package to be installed.")
			fmt.Fprintln(os.Stderr, "Please install it with: pip install buildkite-test-collector.")
			return runnerplugin.InitializeResult{}, fmt.Errorf("--json requires the 'buildkite-test-collector' package to be installed")
		}
		p.useJUnit = false
	}
	if p.useJUnit {
		fmt.Fprintln(os.Stderr, "Buildkite Test Engine Client: Using JUnit XML output for pytest results.")
	} else {
		fmt.Fprintln(os.Stderr, "Buildkite Test Engine Client: Using JSON output for pytest results.")
	}

	if p.cfg.TestFilePattern == "" {
		p.cfg.TestFilePattern = "**/{*_test,test_*}.py"
	}
	if p.cfg.RetryTestCommand == "" {
		p.cfg.RetryTestCommand = p.cfg.TestCommand
	}
	// The collector uploads its own JSON, so bktest only uploads JUnit.
	resultFormat := ""
	if p.useJUnit {
		resultFormat = "junit"
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "pytest",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			FilterTestByTag: true,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
			SplitBySelector: true,
		},
		ResultFormat:   resultFormat,
		ResultPath:     p.cfg.ResultPath,
		LocationPrefix: p.cfg.LocationPrefix,
	}, nil
}

func (p *pytest) Discover() (runnerplugin.DiscoverResult, error) {
	p.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", p.cfg.TestFilePattern, p.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(p.cfg.TestFilePattern, p.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

// Examples lists node IDs with `pytest --collect-only -q`, filtered by
// execution tags when --tag-filters is set.
func (p *pytest) Examples(params runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	if len(params.Files) == 0 {
		return runnerplugin.ExamplesResult{TestCases: []runnerplugin.TestCase{}}, nil
	}
	args := []string{"--collect-only", "-q"}
	if p.cfg.TagFilters != "" {
		args = append(args, "--tag-filters", p.cfg.TagFilters)
	}
	args = append(args, params.Files...)
	output, err := exec.Command("pytest", args...).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return runnerplugin.ExamplesResult{}, fmt.Errorf("pytest collection failed: %s", exitErr.Stderr)
		}
		return runnerplugin.ExamplesResult{}, fmt.Errorf("pytest collection failed: %w", err)
	}
	var result runnerplugin.ExamplesResult
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "::") {
			continue
		}
		result.TestCases = append(result.TestCases, p.nodeIDTestCase(line))
	}
	return result, nil
}

// nodeIDTestCase splits a node ID into scope and name. JUnit XML scopes are
// dotted classnames rather than the collector's file::class scopes.
func (p *pytest) nodeIDTestCase(nodeID string) runnerplugin.TestCase {
	scope, name := "", nodeID
	if i := strings.LastIndex(nodeID, "::"); i != -1 {
		scope, name = nodeID[:i], nodeID[i+2:]
	}
	if p.useJUnit {
		path, rest, hasClass := strings.Cut(scope, "::")
		path = strings.TrimSuffix(path, ".py")
		if hasClass {
			path = path + "::" + rest
		}
		scope = strings.ReplaceAll(strings.ReplaceAll(path, "::", "."), "/", ".")
	}
	return runnerplugin.TestCase{Identifier: nodeID, Path: nodeID, Scope: scope, Name: name, Format: runnerplugin.FormatExample}
}

func (p *pytest) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	cmd := p.cfg.TestCommand
	if params.Retry {
		cmd = p.cfg.RetryTestCommand
	}
	examples := shellquote.Join(runnerplugin.Paths(params.TestCases)...)
	if strings.Contains(cmd, "{{testExamples}}") {
		cmd = strings.Replace(cmd, "{{testExamples}}", examples, 1)
	} else {
		cmd = cmd + " " + examples
	}
	cmd = strings.Replace(cmd, "{{resultPath}}", p.cfg.ResultPath, 1)
	args, err := shellquote.Split(cmd)
	return runnerplugin.CommandResult{Argv: args}, err
}

type collectorTest struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Scope  string            `json:"scope"`
	Result string            `json:"result"`
	Tags   map[string]string `json:"tags"`
}

// Interpret reads the report for exit code 1 (failed tests), and for exit
// code 2 (collection errors) when the collector reports them as tests.
func (p *pytest) Interpret(params runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	var result runnerplugin.InterpretResult
	code := params.ExitCodeOr(0)
	collectionExit := false
	if code != 0 && code != 1 {
		if p.useJUnit || code != 2 {
			return result, nil
		}
		collectionExit = true
	}

	var err error
	if p.useJUnit {
		err = p.interpretJUnit(&result)
	} else {
		err = p.interpretJSON(&result)
	}
	if err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: %v", err))
	}
	if collectionExit && result.Error == "" {
		result.Error = params.Error
	}
	return result, nil
}

func (p *pytest) interpretJSON(result *runnerplugin.InterpretResult) error {
	data, err := os.ReadFile(p.cfg.ResultPath)
	if err != nil {
		return fmt.Errorf("failed to read json: %v", err)
	}
	var tests []collectorTest
	if err := json.Unmarshal(data, &tests); err != nil {
		return fmt.Errorf("failed to parse json: %v", err)
	}
	for _, test := range tests {
		path := test.Name
		if test.Scope != "" {
			path = test.Scope + "::" + test.Name
		}
		result.Tests = append(result.Tests, runnerplugin.TestResult{
			TestCase: runnerplugin.TestCase{Identifier: test.ID, Format: runnerplugin.FormatExample, Scope: test.Scope, Name: test.Name, Path: path},
			Status:   test.Result,
		})
		if test.Tags[collectionErrorTag] == "true" {
			result.Error = fmt.Sprintf("pytest collection failed: %s", path)
		}
	}
	return nil
}

func (p *pytest) interpretJUnit(result *runnerplugin.InterpretResult) error {
	tests, err := junit.Load(p.cfg.ResultPath)
	if err != nil {
		return err
	}
	for _, test := range tests {
		path := nodeIDFromJUnit(test.Classname, test.Name)
		result.Tests = append(result.Tests, runnerplugin.TestResult{
			// JUnit XML ingestion uses the raw classname as the scope.
			TestCase: runnerplugin.TestCase{Identifier: path, Format: runnerplugin.FormatExample, Scope: test.Classname, Name: test.Name, Path: path},
			Status:   test.Result,
		})
	}
	return nil
}

// nodeIDFromJUnit rebuilds a node ID from a dotted JUnit classname. The
// module is the last component named like a test file (test_* or *_test),
// or else everything before the first CamelCase component.
func nodeIDFromJUnit(classname, name string) string {
	if classname == "" {
		return name
	}
	parts := strings.Split(classname, ".")
	moduleEnd := -1
	for i, part := range parts {
		if strings.HasPrefix(part, "test_") || strings.HasSuffix(part, "_test") {
			moduleEnd = i + 1
		}
	}
	if moduleEnd == -1 {
		moduleEnd = len(parts)
		for i, part := range parts {
			if len(part) > 0 && part[0] >= 'A' && part[0] <= 'Z' {
				moduleEnd = i
				break
			}
		}
	}
	module := strings.Join(parts[:moduleEnd], "/") + ".py"
	if classes := parts[moduleEnd:]; len(classes) > 0 {
		return module + "::" + strings.Join(classes, "::") + "::" + name
	}
	return module + "::" + name
}

func tempResultPath(name string) string {
	dir, err := os.MkdirTemp("", "bktec-pytest-*")
	if err != nil {
		panic(err)
	}
	return filepath.Join(dir, name)
}

func pythonPackageInstalled(pkg string) bool {
	output, err := exec.Command("python", "-c", "import importlib.util, sys; print(importlib.util.find_spec(sys.argv[1]) is not None)", pkg).Output()
	return err == nil && strings.TrimSpace(string(output)) == "True"
}

// checkCollectorVersion requires a collector newer than required, for
// --tag-filters support.
func checkCollectorVersion(required string) error {
	output, err := exec.Command("python", "-c", "import importlib.metadata, sys; print(importlib.metadata.version(sys.argv[1]))", "buildkite-test-collector").Output()
	if err != nil {
		return fmt.Errorf("could not determine buildkite-test-collector version: %w", err)
	}
	installed := strings.TrimSpace(string(output))
	if !semver.IsValid("v" + installed) {
		return fmt.Errorf("could not parse installed buildkite-test-collector version %q", installed)
	}
	if semver.Compare("v"+installed, "v"+required) <= 0 {
		return fmt.Errorf("buildkite-test-collector version %s is installed, but version >%s is required for --tag-filters support", installed, required)
	}
	return nil
}
