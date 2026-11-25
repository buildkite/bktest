package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

type jestPlugin struct{}

func main() {
	plugin.Serve(&jestPlugin{})
}

func (p *jestPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "npx jest --runTestsByPath {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}"
	}
	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = "npx jest --testNamePattern '{{testNamePattern}}' --json --testLocationInResults --outputFile {{resultPath}}"
	}
	return plugin.ConfigureResponse{
		RunnerID:           "jest",
		Name:               "Jest",
		Config:             cfg,
		ResultFormat:       "jest-json",
		BatchReportFormat:  "jest-json",
		RequiresResultPath: true,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  false,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       true,
			Mute:            true,
			Skip:            false,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: false,
			InterpretAttempt: true,
			ParseBatchReport: true,
		},
	}, nil
}

func (p *jestPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *jestPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmd := req.Config.TestCommand
	if req.Retry {
		cmd = req.Config.RetryTestCommand
	}

	words, err := plugin.SplitCommand(cmd)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}

	testPaths := plugin.PathsFromTestCases(req.TestCases)
	slices.Sort(testPaths)
	testPaths = slices.Compact(testPaths)

	if req.Retry {
		idx := slices.Index(words, "{{testNamePattern}}")
		if idx < 0 {
			return plugin.PrepareAttemptResponse{}, fmt.Errorf("couldn't find '{{testNamePattern}}' sentinel in retry command")
		}

		escapedTestCases := make([]string, len(req.TestCases))
		for i, tc := range req.TestCases {
			fullName := tc.Name
			if tc.Scope != "" {
				fullName = tc.Scope + " " + tc.Name
			}
			escapedTestCases[i] = regexp.QuoteMeta(fullName)
		}

		testNamePattern := fmt.Sprintf("^(%s)$", strings.Join(escapedTestCases, "|"))
		words = slices.Replace(words, idx, idx+1, testNamePattern)

		testExamplesIdx := slices.Index(words, "{{testExamples}}")
		if testExamplesIdx >= 0 {
			words = slices.Replace(words, testExamplesIdx, testExamplesIdx+1, testPaths...)
		}
	} else {
		idx := slices.Index(words, "{{testExamples}}")
		if idx < 0 {
			words = append(words, testPaths...)
		} else {
			words = slices.Replace(words, idx, idx+1, testPaths...)
		}
	}

	outputIdx := slices.Index(words, "{{resultPath}}")
	if outputIdx < 0 {
		return plugin.PrepareAttemptResponse{}, fmt.Errorf("couldn't find '{{resultPath}}' sentinel in command, exiting")
	}
	words = slices.Replace(words, outputIdx, outputIdx+1, req.Config.ResultPath)

	return plugin.PrepareAttemptResponse{
		Command: words[0],
		Args:    words[1:],
	}, nil
}

func (p *jestPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	report, parseErr := parseJestReport(req.Config.ResultPath)
	if parseErr != nil {
		return plugin.InterpretAttemptResponse{
			ParseWarning: fmt.Sprintf("Buildkite Test Engine Client: Failed to read Jest output, tests will not be retried: %v", parseErr),
		}, nil
	}

	workDir, err := os.Getwd()
	if err != nil {
		return plugin.InterpretAttemptResponse{}, fmt.Errorf("failed to get current working directory: %v", err)
	}

	resp := plugin.InterpretAttemptResponse{}
	for _, testResult := range report.TestResults {
		testPath, err := filepath.Rel(workDir, testResult.FileName)
		if err != nil {
			return plugin.InterpretAttemptResponse{}, fmt.Errorf("failed to get relative path of test file: %v", err)
		}

		if testResult.Status == "failed" && len(testResult.AssertionResults) == 0 {
			resp.ErroredFiles = append(resp.ErroredFiles, testPath)
		}

		for _, example := range testResult.AssertionResults {
			var status string
			switch example.Status {
			case "failed":
				status = plugin.TestStatusFailed
			case "passed":
				status = plugin.TestStatusPassed
			case "pending", "todo":
				status = plugin.TestStatusSkipped
			default:
				status = plugin.TestStatusUnknown
			}

			if req.Retry && status == plugin.TestStatusSkipped {
				continue
			}

			resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
				TestCase: plugin.TestCase{
					Name:  example.Title,
					Scope: strings.Join(example.AncestorTitles, " "),
					Path:  testPath,
				},
				Status: status,
			})
		}
	}
	return resp, nil
}

func (p *jestPlugin) ParseBatchReport(req plugin.ParseBatchReportRequest) (plugin.ParseBatchReportResponse, error) {
	if req.Format != "jest-json" {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("unsupported runner report format %q", req.Format)
	}
	data, err := base64.StdEncoding.DecodeString(req.ReportBase64)
	if err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("decode report: %w", err)
	}

	var report struct {
		NumRuntimeErrorTestSuites int `json:"numRuntimeErrorTestSuites"`
		TestResults               *[]struct {
			AssertionResults []jestExample `json:"assertionResults"`
			FileName         string        `json:"name"`
			Status           string        `json:"status"`
		} `json:"testResults"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("invalid Jest report: %w", err)
	}
	if report.TestResults == nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("incomplete Jest report: missing testResults")
	}

	workDir, err := os.Getwd()
	if err != nil {
		return plugin.ParseBatchReportResponse{}, err
	}

	var resp plugin.ParseBatchReportResponse
	seen := map[string]int{}
	for _, file := range *report.TestResults {
		path := file.FileName
		if filepath.IsAbs(path) {
			if path, err = filepath.Rel(workDir, path); err != nil {
				return plugin.ParseBatchReportResponse{}, fmt.Errorf("failed to get relative path of test file: %w", err)
			}
		}
		path = strings.TrimPrefix(filepath.ToSlash(path), "./")
		if path == "" {
			return plugin.ParseBatchReportResponse{}, fmt.Errorf("jest test file has no name")
		}
		failed := false
		for _, example := range file.AssertionResults {
			var status string
			switch example.Status {
			case "passed":
				status = plugin.TestStatusPassed
			case "failed":
				status = plugin.TestStatusFailed
				failed = true
			case "pending", "skipped", "todo", "disabled":
				status = plugin.TestStatusSkipped
			default:
				return plugin.ParseBatchReportResponse{}, fmt.Errorf("unknown Jest test status %q", example.Status)
			}
			test := plugin.BatchReportedTest{
				TestCase: plugin.TestCase{
					Scope: strings.Join(example.AncestorTitles, " "),
					Name:  example.Title,
					Path:  path,
				},
				Status:   status,
				Selector: path,
			}
			if example.Location.Line > 0 {
				test.Location = fmt.Sprintf("%s:%d", path, example.Location.Line)
			}
			key := strings.Join([]string{path, test.TestCase.Scope, test.TestCase.Name}, "\x00")
			if i, ok := seen[key]; ok {
				if status == plugin.TestStatusFailed || resp.Tests[i].Status == plugin.TestStatusSkipped {
					resp.Tests[i].Status = status
				}
				continue
			}
			seen[key] = len(resp.Tests)
			resp.Tests = append(resp.Tests, test)
		}
		if file.Status == "failed" && !failed {
			resp.ErroredFiles = append(resp.ErroredFiles, path)
		}
	}
	if report.NumRuntimeErrorTestSuites > len(resp.ErroredFiles) {
		resp.ErrorsOutsideTests = true
	}
	return resp, nil
}

type jestExample struct {
	Name           string   `json:"fullName"`
	Status         string   `json:"status"`
	Title          string   `json:"title"`
	AncestorTitles []string `json:"ancestorTitles"`
	Location       struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"location"`
}

type jestReport struct {
	NumFailedTests int `json:"numFailedTests"`
	TestResults    []struct {
		AssertionResults []jestExample `json:"assertionResults"`
		FileName         string        `json:"name"`
		Status           string        `json:"status"`
	} `json:"testResults"`
}

func parseJestReport(path string) (jestReport, error) {
	var report jestReport
	data, err := os.ReadFile(path)
	if err != nil {
		return jestReport{}, fmt.Errorf("failed to read report output: %v", err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return jestReport{}, fmt.Errorf("failed to parse report output: %s", err)
	}
	return report, nil
}
