// Command bktest-runner-rspec is the Go runner plugin for RSpec.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
)

type rspec struct {
	cfg runnerplugin.Config
	log runnerplugin.Logger
}

func main() { runnerplugin.Serve(&rspec{}) }

func (r *rspec) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	r.cfg = params.Config
	r.log.Enabled = r.cfg.Debug
	if r.cfg.TestCommand == "" {
		r.cfg.TestCommand = "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}"
	}
	if r.cfg.TestFilePattern == "" {
		r.cfg.TestFilePattern = "spec/**/*_spec.rb"
	}
	if r.cfg.RetryTestCommand == "" {
		r.cfg.RetryTestCommand = r.cfg.TestCommand
	}
	if r.cfg.LocationPrefix == "" {
		// Test Engine stores RSpec tests with a leading "./".
		r.cfg.LocationPrefix = "./"
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "RSpec",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
			SplitBySelector: true,
		},
		ResultFormat:   "rspec-json",
		ResultPath:     r.cfg.ResultPath,
		LocationPrefix: r.cfg.LocationPrefix,
	}, nil
}

func (r *rspec) Discover() (runnerplugin.DiscoverResult, error) {
	r.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", r.cfg.TestFilePattern, r.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(r.cfg.TestFilePattern, r.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

func (r *rspec) argv(testCases []runnerplugin.TestCase, retry bool) ([]string, error) {
	cmd := r.cfg.TestCommand
	if retry {
		cmd = r.cfg.RetryTestCommand
	}
	words, err := shellquote.Split(cmd)
	if err != nil {
		return nil, err
	}
	paths := runnerplugin.Paths(testCases)
	if idx := slices.Index(words, "{{testExamples}}"); idx < 0 {
		words = append(words, paths...)
	} else {
		words = slices.Replace(words, idx, idx+1, paths...)
	}
	if idx := slices.Index(words, "{{resultPath}}"); idx >= 0 {
		words = slices.Replace(words, idx, idx+1, r.cfg.ResultPath)
	}
	return words, nil
}

func (r *rspec) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	argv, err := r.argv(params.TestCases, params.Retry)
	return runnerplugin.CommandResult{Argv: argv}, err
}

// Examples dry-runs RSpec to list the examples, with their runnable IDs.
func (r *rspec) Examples(params runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	// Users' formatters may not write JSON to stdout, so write it to a file.
	f, err := os.CreateTemp("", "dry-run-*.json")
	if err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to create temporary file for rspec dry run: %v", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	argv, err := r.argv(runnerplugin.TestCasesFromPaths(params.Files), false)
	if err != nil {
		return runnerplugin.ExamplesResult{}, err
	}
	argv = append(argv, "--dry-run", "--format", "json", "--out", f.Name(), "--format", "progress")
	r.log.Printf("Running `%s` for dry run", strings.Join(argv, " "))
	// #nosec G204 -- the test command is configured by the bktest user.
	if output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to run rspec dry run: %s", output)
	}
	report, err := readReport(f.Name())
	if err != nil {
		return runnerplugin.ExamplesResult{}, err
	}
	var result runnerplugin.ExamplesResult
	for _, example := range report.Examples {
		result.TestCases = append(result.TestCases, example.testCase())
	}
	return result, nil
}

// Interpret records each example under its ID, without the leading "./",
// so retries target the exact example even when descriptions repeat.
func (r *rspec) Interpret(runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	var result runnerplugin.InterpretResult
	report, err := readReport(r.cfg.ResultPath)
	if err != nil {
		// A missing report must not fail the build by itself: the command's
		// exit status still does.
		result.Messages = append(result.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried: %v", err))
		return result, nil
	}
	for _, example := range report.Examples {
		testCase := example.testCase()
		result.Tests = append(result.Tests, runnerplugin.TestResult{
			TestCase: testCase,
			Status:   status(example.Status),
			Key:      strings.TrimPrefix(testCase.Identifier, "./"),
		})
	}
	if n := report.Summary.ErrorsOutsideOfExamplesCount; n > 0 {
		result.Error = fmt.Sprintf("RSpec reported %d errors outside of examples", n)
	}
	return result, nil
}

type example struct {
	ID              string `json:"id"`
	Description     string `json:"description"`
	FullDescription string `json:"full_description"`
	Status          string `json:"status"`
}

type report struct {
	Examples []example `json:"examples"`
	Summary  struct {
		ErrorsOutsideOfExamplesCount int `json:"errors_outside_of_examples_count"`
	} `json:"summary"`
}

func readReport(path string) (report, error) {
	var r report
	data, err := os.ReadFile(path)
	if err != nil {
		return r, fmt.Errorf("failed to read rspec output: %v", err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("failed to parse rspec output: %s", err)
	}
	return r, nil
}

// testCase matches the Buildkite Ruby test collector's identity: its scope is
// the example group's description, which the JSON report only has as the
// full description with the example's own description at the end.
func (e example) testCase() runnerplugin.TestCase {
	return runnerplugin.TestCase{
		Identifier: e.ID,
		Name:       e.Description,
		Path:       e.ID,
		Scope:      strings.TrimSuffix(e.FullDescription, " "+e.Description),
	}
}

func status(s string) string {
	switch s {
	case "passed":
		return runnerplugin.StatusPassed
	case "failed":
		return runnerplugin.StatusFailed
	case "pending":
		return runnerplugin.StatusSkipped
	default:
		return runnerplugin.StatusUnknown
	}
}
