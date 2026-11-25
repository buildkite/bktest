// Command bktest-runner-gotest is the Go runner plugin for go test.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin/junit"
	"github.com/kballard/go-shellquote"
)

const (
	formatJUnit   = "junit"
	formatGoJSONL = "go-jsonl"
)

type gotest struct {
	cfg    runnerplugin.Config
	log    runnerplugin.Logger
	format string
}

func main() { runnerplugin.Serve(&gotest{}) }

func (g *gotest) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	g.cfg = params.Config
	g.log.Enabled = g.cfg.Debug
	if g.cfg.TestCommand == "" {
		g.cfg.TestCommand = "gotestsum --jsonfile={{resultPath}} {{packages}}"
	}
	if g.cfg.RetryTestCommand == "" {
		g.cfg.RetryTestCommand = g.cfg.TestCommand
	}
	g.format = formatJUnit
	if commandProducesGoJSONL(g.cfg.TestCommand) {
		g.format = formatGoJSONL
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "gotest",
		Features: runnerplugin.Features{
			SplitBySelector: true,
			AutoRetry:       true,
			Mute:            true,
		},
		ResultFormat:   g.format,
		ResultPath:     g.cfg.ResultPath,
		LocationPrefix: g.cfg.LocationPrefix,
	}, nil
}

// Discover lists the packages that have test files: packages are the
// smallest unit go test can split on.
func (g *gotest) Discover() (runnerplugin.DiscoverResult, error) {
	g.log.Printf("Discovering Go packages with tests using `go list`")
	output, err := exec.Command("go", "list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", "./...").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return runnerplugin.DiscoverResult{}, fmt.Errorf("go list failed: %w\nstderr:\n%s", err, string(ee.Stderr))
		}
		return runnerplugin.DiscoverResult{}, fmt.Errorf("failed to run go list: %w", err)
	}
	packages := []string{}
	for _, pkg := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if pkg != "" {
			packages = append(packages, pkg)
		}
	}
	g.log.Printf("Discovered %d packages with tests", len(packages))
	if len(packages) == 0 {
		return runnerplugin.DiscoverResult{}, fmt.Errorf("no Go packages with tests found using `go list`")
	}
	return runnerplugin.DiscoverResult{Targets: packages}, nil
}

func (g *gotest) Examples(runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	return runnerplugin.ExamplesResult{}, errors.New("go test does not support split by example")
}

func (g *gotest) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	var packages []string
	seen := map[string]bool{}
	for _, tc := range params.TestCases {
		pkg := tc.Path
		if (tc.Format == runnerplugin.FormatSelector && tc.Value != "") || (tc.Value != "" && tc.Path == "") {
			pkg = tc.Value
		}
		if pkg != "" && !seen[pkg] {
			packages = append(packages, pkg)
			seen[pkg] = true
		}
	}
	if len(packages) == 0 {
		return runnerplugin.CommandResult{}, fmt.Errorf("failed to generate test package list: unable to extract package names from test plan")
	}
	g.log.Printf("Packages: %v", packages)

	cmd := g.cfg.TestCommand
	if params.Retry {
		cmd = g.cfg.RetryTestCommand
	}
	joined := strings.Join(packages, " ")
	if strings.Contains(cmd, "{{packages}}") {
		cmd = strings.Replace(cmd, "{{packages}}", joined, 1)
	} else {
		cmd = cmd + " " + joined
	}
	cmd = strings.Replace(cmd, "{{resultPath}}", g.cfg.ResultPath, 1)
	args, err := shellquote.Split(cmd)
	if err != nil {
		return runnerplugin.CommandResult{}, err
	}
	result := runnerplugin.CommandResult{Argv: args}
	// `go test -json` writes its report to stdout, so bktest saves a copy.
	if g.format == formatGoJSONL && isGoTestJSONCommand(args) && jsonFileArg(args) == "" {
		result.StdoutPath = g.cfg.ResultPath
	}
	return result, nil
}

func (g *gotest) Interpret(params runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	var result runnerplugin.InterpretResult
	// go test exits 1 for both build and test failures, and other codes for
	// everything else, which leaves no report worth reading.
	if code := params.ExitCodeOr(0); code != 0 && code != 1 {
		return result, nil
	}
	var err error
	if g.format == formatGoJSONL {
		err = g.interpretGoJSONL(&result)
	} else {
		err = g.interpretJUnit(&result)
	}
	if err != nil {
		result = runnerplugin.InterpretResult{Messages: []string{fmt.Sprintf("Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: %v", err)}}
	}
	return result, nil
}

func (g *gotest) interpretJUnit(result *runnerplugin.InterpretResult) error {
	tests, err := junit.Load(g.cfg.ResultPath)
	if err != nil {
		return err
	}
	for _, test := range tests {
		// gotestsum reports a package that fails to build as a "TestMain"
		// with no classname. Retrying can't fix it, so it errors the run.
		if test.Name == "TestMain" && test.Classname == "" && test.Failure != nil && strings.Contains(test.Failure.Content, "[build failed]") {
			result.Error = fmt.Sprintf("go test failed to build %s", test.SuiteName)
			continue
		}
		result.Tests = append(result.Tests, runnerplugin.TestResult{
			TestCase: runnerplugin.TestCase{Format: runnerplugin.FormatExample, Scope: test.Classname, Name: test.Name, Path: test.Classname},
			Status:   test.Result,
		})
	}
	return nil
}

type event struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	FailedBuild string `json:"FailedBuild"`
}

func (g *gotest) interpretGoJSONL(result *runnerplugin.InterpretResult) error {
	file, err := os.Open(g.cfg.ResultPath)
	if err != nil {
		return fmt.Errorf("failed to read go jsonl: %w", err)
	}
	defer file.Close()
	var events []event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return fmt.Errorf("failed to parse go jsonl: %w", err)
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read go jsonl: %w", err)
	}
	if len(events) == 0 {
		return fmt.Errorf("failed to parse go jsonl: no events found")
	}

	outputs := map[string]string{}
	var failedPackages []string
	failed, buildFailed, hasFailedTest := map[string]bool{}, map[string]bool{}, map[string]bool{}
	statuses := map[string]string{}
	var keys []string
	cases := map[string]runnerplugin.TestCase{}
	for _, e := range events {
		if e.Package == "" {
			continue
		}
		outputs[e.Package] += e.Output
		if e.Test == "" {
			if e.Action == "fail" {
				if !failed[e.Package] {
					failedPackages = append(failedPackages, e.Package)
				}
				failed[e.Package] = true
				if e.FailedBuild != "" {
					buildFailed[e.Package] = true
				}
			}
			continue
		}
		var status string
		switch e.Action {
		case "pass":
			status = runnerplugin.StatusPassed
		case "fail":
			status = runnerplugin.StatusFailed
			hasFailedTest[e.Package] = true
		case "skip":
			status = runnerplugin.StatusSkipped
		default:
			continue
		}
		key := e.Package + "/" + e.Test
		if _, ok := cases[key]; !ok {
			keys = append(keys, key)
		}
		if statuses[key] != runnerplugin.StatusFailed {
			statuses[key] = status
		}
		cases[key] = runnerplugin.TestCase{Format: runnerplugin.FormatExample, Scope: e.Package, Name: e.Test, Path: e.Package}
	}
	for _, key := range keys {
		result.Tests = append(result.Tests, runnerplugin.TestResult{TestCase: cases[key], Status: statuses[key]})
	}
	for _, pkg := range failedPackages {
		if buildFailed[pkg] || strings.Contains(outputs[pkg], "[build failed]") {
			result.Error = fmt.Sprintf("go test failed to build %s", pkg)
			continue
		}
		// A package that failed without a failed test, such as a crashing
		// TestMain, is reported as its TestMain so it can be retried.
		if !hasFailedTest[pkg] {
			result.Tests = append(result.Tests, runnerplugin.TestResult{
				TestCase: runnerplugin.TestCase{Format: runnerplugin.FormatExample, Scope: pkg, Name: "TestMain", Path: pkg},
				Status:   runnerplugin.StatusFailed,
			})
		}
	}
	return nil
}

func commandProducesGoJSONL(command string) bool {
	args, err := shellquote.Split(command)
	if err != nil {
		return strings.Contains(command, "--jsonfile") || strings.Contains(command, "-json")
	}
	return isGoTestJSONCommand(args) || jsonFileArg(args) != ""
}

func isGoTestJSONCommand(args []string) bool {
	if len(args) < 3 || filepath.Base(args[0]) != "go" || args[1] != "test" {
		return false
	}
	for _, arg := range args {
		if arg == "-json" {
			return true
		}
	}
	return false
}

func jsonFileArg(args []string) string {
	for i, arg := range args {
		if arg == "--jsonfile" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(arg, "--jsonfile="); ok {
			return v
		}
	}
	return ""
}
