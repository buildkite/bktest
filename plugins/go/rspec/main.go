package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

type rspecPlugin struct{}

func main() {
	plugin.Serve(&rspecPlugin{})
}

func (p *rspecPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}"
	}
	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "spec/**/*_spec.rb"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}
	if cfg.LocationPrefix == "" {
		cfg.LocationPrefix = "./"
	}
	return plugin.ConfigureResponse{
		RunnerID:           "rspec",
		Name:               "RSpec",
		Config:             cfg,
		ResultFormat:       "rspec-json",
		BatchReportFormat:  "rspec-json",
		RequiresResultPath: true,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  true,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: true,
			InterpretAttempt: true,
			ParseBatchReport: true,
		},
	}, nil
}

func (p *rspecPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *rspecPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmdName, cmdArgs, err := commandNameAndArgs(req.Config, req.TestCases, req.Retry)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}
	return plugin.PrepareAttemptResponse{
		Command: cmdName,
		Args:    cmdArgs,
	}, nil
}

func (p *rspecPlugin) DiscoverExamples(req plugin.DiscoverExamplesRequest) (plugin.DiscoverExamplesResponse, error) {
	f, err := os.CreateTemp("", "dry-run-*.json")
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to create temporary file for rspec dry run: %v", err)
	}
	tmpPath := f.Name()
	f.Close()
	defer os.Remove(tmpPath)

	cmdName, cmdArgs, err := commandNameAndArgs(req.Config, plugin.TestCasesFromPaths(req.Files), false)
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, err
	}
	cmdArgs = append(cmdArgs, "--dry-run", "--format", "json", "--out", tmpPath, "--format", "progress")

	output, err := exec.Command(cmdName, cmdArgs...).CombinedOutput()
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to run rspec dry run: %s", output)
	}

	report, err := parseReportFile(tmpPath)
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, err
	}

	examples := make([]plugin.TestCase, 0, len(report.Examples))
	for _, example := range report.Examples {
		examples = append(examples, mapExampleToTestCase(example))
	}
	return plugin.DiscoverExamplesResponse{Examples: examples}, nil
}

func (p *rspecPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	report, err := parseReportFile(req.Config.ResultPath)
	if err != nil {
		return plugin.InterpretAttemptResponse{
			ParseWarning: fmt.Sprintf("Buildkite Test Engine Client: Failed to read Rspec output, tests will not be retried: %v", err),
		}, nil
	}

	resp := plugin.InterpretAttemptResponse{
		Results: make([]plugin.AttemptTestOutcome, 0, len(report.Examples)),
	}
	for _, example := range report.Examples {
		tc := mapExampleToTestCase(example)
		key := strings.TrimPrefix(tc.Identifier, "./")
		if key == "" {
			key = plugin.TestIdentifier(tc)
		}
		resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
			Key:      key,
			TestCase: tc,
			Status:   rspecExampleStatus(example.Status),
		})
	}
	if report.Summary.ErrorsOutsideOfExamplesCount > 0 {
		resp.RunError = fmt.Sprintf("RSpec reported %d errors outside of examples", report.Summary.ErrorsOutsideOfExamplesCount)
	}
	return resp, nil
}

func (p *rspecPlugin) ParseBatchReport(req plugin.ParseBatchReportRequest) (plugin.ParseBatchReportResponse, error) {
	if req.Format != "rspec-json" {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("unsupported runner report format %q", req.Format)
	}
	data, err := base64.StdEncoding.DecodeString(req.ReportBase64)
	if err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("decode report: %w", err)
	}
	report, err := parseRspecReport(data)
	if err != nil {
		return plugin.ParseBatchReportResponse{}, err
	}
	var shape struct {
		Examples *json.RawMessage `json:"examples"`
		Summary  *struct {
			ExampleCount *int `json:"example_count"`
		} `json:"summary"`
	}
	if json.Unmarshal(data, &shape) != nil || shape.Examples == nil || shape.Summary == nil || shape.Summary.ExampleCount == nil || report.Summary.ExampleCount != len(report.Examples) {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("incomplete RSpec report or mismatched example count")
	}

	resp := plugin.ParseBatchReportResponse{
		ErrorsOutsideTests: report.Summary.ErrorsOutsideOfExamplesCount != 0,
	}
	for _, example := range report.Examples {
		status := rspecExampleStatus(example.Status)
		if status == plugin.TestStatusUnknown {
			return plugin.ParseBatchReportResponse{}, fmt.Errorf("unknown RSpec example status %q", example.Status)
		}
		tc := mapExampleToTestCase(example)
		tc.Identifier = strings.TrimPrefix(tc.Identifier, "./")
		tc.Path = strings.TrimPrefix(tc.Path, "./")
		location := ""
		if example.FilePath != "" && example.LineNumber > 0 {
			location = fmt.Sprintf("%s:%d", strings.TrimPrefix(example.FilePath, "./"), example.LineNumber)
		}
		if tc.Path == "" {
			tc.Path = location
		}
		if tc.Path == "" {
			return plugin.ParseBatchReportResponse{}, fmt.Errorf("RSpec example has no runnable ID or file:line")
		}
		resp.Tests = append(resp.Tests, plugin.BatchReportedTest{
			TestCase: tc,
			Status:   status,
			Selector: strings.TrimPrefix(example.primarySelector(), "./"),
			Location: location,
		})
	}
	return resp, nil
}

type rspecExample struct {
	ID              string  `json:"id"`
	Description     string  `json:"description"`
	FullDescription string  `json:"full_description"`
	Status          string  `json:"status"`
	FilePath        string  `json:"file_path"`
	LineNumber      int     `json:"line_number"`
	RunTime         float64 `json:"run_time"`
}

func (e rspecExample) primarySelector() string {
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

type rspecReport struct {
	Version  string         `json:"version"`
	Seed     int            `json:"seed"`
	Examples []rspecExample `json:"examples"`
	Summary  struct {
		ExampleCount                 int `json:"example_count"`
		FailureCount                 int `json:"failure_count"`
		PendingCount                 int `json:"pending_count"`
		ErrorsOutsideOfExamplesCount int `json:"errors_outside_of_examples_count"`
	}
}

func parseReportFile(path string) (rspecReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return rspecReport{}, fmt.Errorf("failed to read rspec output: %v", err)
	}
	return parseRspecReport(data)
}

func parseRspecReport(data []byte) (rspecReport, error) {
	var report rspecReport
	if err := json.Unmarshal(data, &report); err != nil {
		return rspecReport{}, fmt.Errorf("failed to parse rspec output: %s", err)
	}
	return report, nil
}

func rspecExampleStatus(status string) string {
	switch status {
	case "passed":
		return plugin.TestStatusPassed
	case "failed":
		return plugin.TestStatusFailed
	case "pending":
		return plugin.TestStatusSkipped
	default:
		return plugin.TestStatusUnknown
	}
}

func mapExampleToTestCase(example rspecExample) plugin.TestCase {
	scope := strings.TrimSuffix(example.FullDescription, " "+example.Description)
	return plugin.TestCase{
		Identifier: example.ID,
		Name:       example.Description,
		Path:       example.ID,
		Scope:      scope,
	}
}

func commandNameAndArgs(cfg plugin.RunnerSettings, testCases []plugin.TestCase, retry bool) (string, []string, error) {
	cmd := cfg.TestCommand
	if retry {
		cmd = cfg.RetryTestCommand
	}
	words, err := plugin.SplitCommand(cmd)
	if err != nil {
		return "", []string{}, err
	}

	testPaths := plugin.PathsFromTestCases(testCases)

	idx := slices.Index(words, "{{testExamples}}")
	if idx < 0 {
		words = append(words, testPaths...)
	} else {
		words = slices.Replace(words, idx, idx+1, testPaths...)
	}

	idx = slices.Index(words, "{{resultPath}}")
	if idx >= 0 {
		words = slices.Replace(words, idx, idx+1, cfg.ResultPath)
	}

	return words[0], words[1:], nil
}
