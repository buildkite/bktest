// Package rspec is the runner plugin for RSpec.
package rspec

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

type handler struct{}

// New returns the RSpec handler.
func New() runnerplugin.Handler { return handler{} }

var (
	_ runnerplugin.ExampleDiscoverer  = handler{}
	_ runnerplugin.AttemptInterpreter = handler{}
	_ runnerplugin.BatchReportParser  = handler{}
)

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	if c.TestCommand == "" {
		c.TestCommand = "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}"
	}
	if c.TestFilePattern == "" {
		c.TestFilePattern = "spec/**/*_spec.rb"
	}
	if c.RetryTestCommand == "" {
		c.RetryTestCommand = c.TestCommand
	}
	if c.LocationPrefix == "" {
		// rspec test in Test Engine is stored with leading "./"
		// therefore, we need to add "./" to the file path
		// to match the test path in Test Engine
		c.LocationPrefix = "./"
	}
	return runnerplugin.ConfigureResponse{
		Name:   "RSpec",
		Config: c,
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
			SplitBySelector: true,
		},
		ResultFormat: "rspec-json",
	}, nil
}

// commandWords replaces the "{{testExamples}}" placeholder in the test
// command with the test cases, and "{{resultPath}}" with the result path.
func commandWords(c runnerplugin.Config, testCases []runnerplugin.TestCase, retry bool) ([]string, error) {
	cmd := c.TestCommand
	if retry {
		cmd = c.RetryTestCommand
	}
	words, err := shellquote.Split(cmd)
	if err != nil {
		return nil, err
	}
	testPaths := runnerplugin.Paths(testCases)
	if idx := slices.Index(words, "{{testExamples}}"); idx < 0 {
		words = append(words, testPaths...)
	} else {
		words = slices.Replace(words, idx, idx+1, testPaths...)
	}
	if idx := slices.Index(words, "{{resultPath}}"); idx >= 0 {
		words = slices.Replace(words, idx, idx+1, c.ResultPath)
	}
	return words, nil
}

func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	words, err := commandWords(request.Config, request.TestCases, request.Retry)
	return runnerplugin.PrepareAttemptResponse{Argv: words}, err
}

// DiscoverExamples dry-runs RSpec to list the examples in the files.
func (handler) DiscoverExamples(request runnerplugin.DiscoverExamplesRequest) (runnerplugin.DiscoverExamplesResponse, error) {
	var response runnerplugin.DiscoverExamplesResponse
	// Create a temporary file to store the JSON output of the rspec dry run.
	// We cannot simply read the dry run output from stdout because
	// users may have custom formatters that do not output JSON.
	f, err := os.CreateTemp("", "dry-run-*.json")
	if err != nil {
		return response, fmt.Errorf("failed to create temporary file for rspec dry run: %v", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	words, err := commandWords(request.Config, runnerplugin.TestCasesFromPaths(request.Files), false)
	if err != nil {
		return response, err
	}
	words = append(words, "--dry-run", "--format", "json", "--out", f.Name(), "--format", "progress")
	request.Config.Debugf("Running `%s` for dry run", strings.Join(words, " "))

	// #nosec G204 -- the test command is configured by the bktest user.
	output, err := exec.Command(words[0], words[1:]...).CombinedOutput()
	if err != nil {
		return response, fmt.Errorf("failed to run rspec dry run: %s", output)
	}

	report, err := ReadReport(f.Name())
	if err != nil {
		return response, err
	}
	for _, example := range report.Examples {
		response.TestCases = append(response.TestCases, example.testCase())
	}
	return response, nil
}

// InterpretAttempt records each example under its ID, without the leading
// "./", so retries target the exact example even when descriptions repeat.
func (handler) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error) {
	var response runnerplugin.InterpretAttemptResponse
	// RSpec exits with a non-zero status code when there are test failures,
	// so we always attempt to parse the report.
	report, err := ReadReport(request.Config.ResultPath)
	if err != nil {
		// We don't want to fail the build if we fail to parse the report:
		// the command's own error still does.
		response.Messages = append(response.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried: %v", err))
		return response, nil
	}
	for _, example := range report.Examples {
		testCase := example.testCase()
		response.Tests = append(response.Tests, runnerplugin.TestResult{
			TestCase: testCase,
			Status:   exampleStatus(example.Status),
			Key:      strings.TrimPrefix(testCase.Identifier, "./"),
		})
	}
	if n := report.Summary.ErrorsOutsideOfExamplesCount; n > 0 {
		response.RunError = fmt.Sprintf("RSpec reported %d errors outside of examples", n)
	}
	return response, nil
}

// Example represents a single test example in an RSpec report.
type Example struct {
	ID              string  `json:"id"`
	Description     string  `json:"description"`
	FullDescription string  `json:"full_description"`
	Status          string  `json:"status"`
	FilePath        string  `json:"file_path"`
	LineNumber      int     `json:"line_number"`
	RunTime         float64 `json:"run_time"`
}

// PrimarySelector is the spec file RSpec ran, which can differ from file_path
// when an example is defined in a shared-examples file. RSpec IDs end in a
// numeric example-group location such as [1:2:1].
func (e Example) PrimarySelector() string {
	start := strings.LastIndexByte(e.ID, '[')
	if start > 0 && strings.HasSuffix(e.ID, "]") && start+1 < len(e.ID)-1 {
		for _, c := range e.ID[start+1 : len(e.ID)-1] {
			if c != ':' && (c < '0' || c > '9') {
				return e.FilePath
			}
		}
		return e.ID[:start]
	}
	return e.FilePath
}

// testCase maps an example to a test case. The scope and name have to match
// the Buildkite test collector's. The collector's scope is the example
// group's full description, which the RSpec JSON report doesn't have: it only
// has the example's full_description, which ends with the example's own
// description, so that is removed to get the scope.
// See https://github.com/buildkite/test-collector-ruby/blob/2d641486e42f666dd07ffed4cbf2cd0f9dc97619/lib/buildkite/test_collector/rspec_plugin/trace.rb#L27
func (e Example) testCase() runnerplugin.TestCase {
	return runnerplugin.TestCase{
		Identifier: e.ID,
		Name:       e.Description,
		Path:       e.ID,
		Scope:      strings.TrimSuffix(e.FullDescription, " "+e.Description),
	}
}

// Report is the structure of RSpec's JSON report.
type Report struct {
	Version  string    `json:"version"`
	Seed     int       `json:"seed"`
	Examples []Example `json:"examples"`
	Summary  struct {
		ExampleCount                 int `json:"example_count"`
		FailureCount                 int `json:"failure_count"`
		PendingCount                 int `json:"pending_count"`
		ErrorsOutsideOfExamplesCount int `json:"errors_outside_of_examples_count"`
	}
}

// ReadReport reads the RSpec JSON report at path.
func ReadReport(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("failed to read rspec output: %v", err)
	}
	return ParseReport(data)
}

// ParseReport decodes an RSpec JSON report.
func ParseReport(data []byte) (Report, error) {
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, fmt.Errorf("failed to parse rspec output: %s", err)
	}
	return report, nil
}

func exampleStatus(status string) string {
	switch status {
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

// ParseBatchReport handles a persistent runner's inline report. Unlike
// InterpretAttempt, it validates that the report is complete and maps
// examples to canonical selectors for lease matching and retries.
func (handler) ParseBatchReport(request runnerplugin.ParseBatchReportRequest) (runnerplugin.ParseBatchReportResponse, error) {
	var parsed runnerplugin.ParseBatchReportResponse
	if request.Format != "rspec-json" {
		return parsed, fmt.Errorf("unsupported runner report format %q", request.Format)
	}
	data, err := os.ReadFile(request.ReportPath)
	if err != nil {
		return parsed, err
	}
	report, err := ParseReport(data)
	if err != nil {
		return parsed, err
	}
	var shape struct {
		Examples *json.RawMessage `json:"examples"`
		Summary  *struct {
			ExampleCount *int `json:"example_count"`
		} `json:"summary"`
	}
	if json.Unmarshal(data, &shape) != nil || shape.Examples == nil || shape.Summary == nil || shape.Summary.ExampleCount == nil || report.Summary.ExampleCount != len(report.Examples) {
		return parsed, fmt.Errorf("incomplete RSpec report or mismatched example count")
	}
	parsed.ErrorsOutsideTests = report.Summary.ErrorsOutsideOfExamplesCount != 0
	for _, example := range report.Examples {
		status := exampleStatus(example.Status)
		if status == runnerplugin.StatusUnknown {
			return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("unknown RSpec example status %q", example.Status)
		}
		caseResult := example.testCase()
		caseResult.Identifier = strings.TrimPrefix(caseResult.Identifier, "./")
		caseResult.Path = strings.TrimPrefix(caseResult.Path, "./")
		location := ""
		if example.FilePath != "" && example.LineNumber > 0 {
			location = fmt.Sprintf("%s:%d", strings.TrimPrefix(example.FilePath, "./"), example.LineNumber)
		}
		if caseResult.Path == "" {
			caseResult.Path = location
		}
		if caseResult.Path == "" {
			return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("RSpec example has no runnable ID or file:line")
		}
		parsed.Tests = append(parsed.Tests, runnerplugin.BatchTest{TestCase: caseResult, Status: status, Selector: strings.TrimPrefix(example.PrimarySelector(), "./"), Location: location})
	}
	return parsed, nil
}
