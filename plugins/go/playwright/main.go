package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

var playwrightLineSuffix = regexp.MustCompile(`:\d+$`)

type playwrightPlugin struct{}

func main() {
	plugin.Serve(&playwrightPlugin{})
}

func (p *playwrightPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "npx playwright test"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}
	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "**/{*.spec,*.test}.{ts,js}"
	}
	resultFormat := ""
	if cfg.ResultPath != "" {
		resultFormat = "playwright-json"
	}
	return plugin.ConfigureResponse{
		RunnerID:           "playwright",
		Name:               "Playwright",
		Config:             cfg,
		ResultFormat:       resultFormat,
		BatchReportFormat:  "playwright-json",
		RequiresResultPath: true,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  true,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       true,
			Mute:            true,
			Skip:            false,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: true,
			InterpretAttempt: true,
			ParseBatchReport: true,
		},
	}, nil
}

func (p *playwrightPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *playwrightPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmdName, cmdArgs, err := commandNameAndArgs(req.Config, req.TestCases, req.Retry)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}
	return plugin.PrepareAttemptResponse{
		Command: cmdName,
		Args:    cmdArgs,
	}, nil
}

func (p *playwrightPlugin) DiscoverExamples(req plugin.DiscoverExamplesRequest) (plugin.DiscoverExamplesResponse, error) {
	if len(req.Files) == 0 {
		return plugin.DiscoverExamplesResponse{Examples: []plugin.TestCase{}}, nil
	}

	tmpFile, err := os.CreateTemp("", "playwright-list-*.json")
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	cmdName, cmdArgs, err := commandNameAndArgs(req.Config, plugin.TestCasesFromPaths(req.Files), false)
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, err
	}
	cmdArgs = append(cmdArgs, "--list", "--reporter=json")

	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PLAYWRIGHT_JSON_OUTPUT_NAME=%s", tmpPath))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to list playwright tests: %w\nstderr: %s", err, stderr.String())
	}

	report, err := parsePlaywrightReport(tmpPath)
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to parse playwright test list output: %w", err)
	}

	var examples []plugin.TestCase
	for _, suite := range report.Suites {
		examples = append(examples, getTestCasesFromSuite(suite, suite.Title)...)
	}
	return plugin.DiscoverExamplesResponse{Examples: examples}, nil
}

func (p *playwrightPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	report, parseErr := parsePlaywrightReport(req.Config.ResultPath)
	if parseErr != nil {
		return plugin.InterpretAttemptResponse{
			ParseWarning: fmt.Sprintf("Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: %v", parseErr),
		}, nil
	}

	resp := plugin.InterpretAttemptResponse{}
	for _, suite := range report.Suites {
		resp.Results = append(resp.Results, getTestResultsFromSuite(suite, suite.Title)...)
	}

	if len(report.Errors) == 0 {
		return resp, nil
	}

	files, ok := playwrightErrorFiles(report.Errors)
	unmatched := slices.Clone(files)
	var remaining []plugin.TestCase
	for _, tc := range req.TestCases {
		file := tc.Path
		if tc.Format == plugin.TestCaseFormatSelector {
			file = tc.Value
		}
		file = strings.TrimPrefix(playwrightLineSuffix.ReplaceAllString(file, ""), "./")
		if slices.Contains(files, file) {
			unmatched = slices.DeleteFunc(unmatched, func(f string) bool { return f == file })
		} else {
			remaining = append(remaining, tc)
		}
	}

	if !ok || len(report.Suites) > 0 || len(unmatched) > 0 {
		resp.RunError = "Playwright failed with errors"
		return resp, nil
	}

	resp.ErroredFiles = files
	if len(remaining) > 0 {
		resp.RerunTestCases = remaining
		resp.RerunMessage = fmt.Sprintf("+++ Buildkite Test Engine Client: Playwright failed to load %s. Running the other test files again.", strings.Join(files, ", "))
	}
	return resp, nil
}

func (p *playwrightPlugin) ParseBatchReport(req plugin.ParseBatchReportRequest) (plugin.ParseBatchReportResponse, error) {
	if req.Format != "playwright-json" {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("unsupported runner report format %q", req.Format)
	}
	data, err := base64.StdEncoding.DecodeString(req.ReportBase64)
	if err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("decode report: %w", err)
	}

	var report struct {
		Config struct {
			RootDir string `json:"rootDir"`
		} `json:"config"`
		Suites *[]playwrightReportSuite `json:"suites"`
		Errors []playwrightError        `json:"errors"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("invalid Playwright report: %w", err)
	}
	if report.Suites == nil || report.Config.RootDir == "" {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("incomplete Playwright report: missing suites or config.rootDir")
	}

	workDir, err := os.Getwd()
	if err != nil {
		return plugin.ParseBatchReportResponse{}, err
	}

	resp := plugin.ParseBatchReportResponse{
		UnselectedTests: true,
	}
	seen := map[string]int{}
	var walk func(suite playwrightReportSuite, titles string) error
	walk = func(suite playwrightReportSuite, titles string) error {
		for _, spec := range suite.Specs {
			if spec.File == "" {
				return fmt.Errorf("playwright test %q has no file", spec.Title)
			}
			path, err := filepath.Rel(workDir, filepath.Join(report.Config.RootDir, spec.File))
			if err != nil {
				return fmt.Errorf("failed to get relative path of test file: %w", err)
			}
			path = filepath.ToSlash(path)
			for _, run := range spec.Tests {
				var status string
				switch run.Status {
				case "expected", "flaky":
					status = plugin.TestStatusPassed
				case "unexpected":
					status = plugin.TestStatusFailed
				case "skipped":
					status = plugin.TestStatusSkipped
					if run.ExpectedStatus != "skipped" && !slices.Contains(resp.ErroredFiles, path) {
						resp.ErroredFiles = append(resp.ErroredFiles, path)
					}
				default:
					return fmt.Errorf("unknown Playwright test status %q", run.Status)
				}
				test := plugin.BatchReportedTest{
					TestCase: plugin.TestCase{
						Scope: fmt.Sprintf(" %s %s %s", run.ProjectName, titles, spec.Title),
						Name:  spec.Title,
						Path:  fmt.Sprintf("%s:%d", path, spec.Line),
					},
					Status:   status,
					Selector: path,
				}
				key := strings.Join([]string{path, test.TestCase.Scope}, "\x00")
				if i, ok := seen[key]; ok {
					if status == plugin.TestStatusFailed || resp.Tests[i].Status == plugin.TestStatusSkipped {
						resp.Tests[i].Status = status
					}
					continue
				}
				seen[key] = len(resp.Tests)
				resp.Tests = append(resp.Tests, test)
			}
		}
		for _, child := range suite.Suites {
			if err := walk(child, titles+" "+child.Title); err != nil {
				return err
			}
		}
		return nil
	}
	for _, suite := range *report.Suites {
		if err := walk(suite, suite.Title); err != nil {
			return plugin.ParseBatchReportResponse{}, err
		}
	}
	if len(report.Errors) > 0 {
		files, ok := playwrightErrorFiles(report.Errors)
		if ok && len(resp.Tests) == 0 {
			resp.ErroredFiles = files
			resp.ErroredFilesStoppedRun = true
		} else {
			resp.ErrorsOutsideTests = true
		}
	}
	return resp, nil
}

type playwrightTest struct {
	ProjectName    string `json:"projectName"`
	Status         string `json:"status"`
	ExpectedStatus string `json:"expectedStatus"`
}

type playwrightSpec struct {
	File   string           `json:"file"`
	Line   int              `json:"line"`
	Column int              `json:"column"`
	ID     string           `json:"id"`
	Title  string           `json:"title"`
	Ok     bool             `json:"ok"`
	Tests  []playwrightTest `json:"tests"`
}

type playwrightReportSuite struct {
	Title  string                  `json:"title"`
	Specs  []playwrightSpec        `json:"specs"`
	Suites []playwrightReportSuite `json:"suites"`
}

type playwrightError struct {
	Message  string `json:"message"`
	Location *struct {
		File string `json:"file"`
	} `json:"location"`
}

type playwrightReport struct {
	Suites []playwrightReportSuite `json:"suites"`
	Errors []playwrightError       `json:"errors"`
}

func parsePlaywrightReport(path string) (playwrightReport, error) {
	var report playwrightReport
	data, err := os.ReadFile(path)
	if err != nil {
		return playwrightReport{}, fmt.Errorf("failed to read playwright output: %v", err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return playwrightReport{}, fmt.Errorf("failed to parse playwright output: %s", err)
	}
	return report, nil
}

func playwrightErrorFiles(errs []playwrightError) ([]string, bool) {
	workDir, err := os.Getwd()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, e := range errs {
		if e.Location == nil || e.Location.File == "" {
			return nil, false
		}
		file, err := filepath.Rel(workDir, e.Location.File)
		if err != nil {
			return nil, false
		}
		file = filepath.ToSlash(file)
		if !slices.Contains(files, file) {
			files = append(files, file)
		}
	}
	return files, true
}

func getTestResultsFromSuite(suite playwrightReportSuite, suiteName string) []plugin.AttemptTestOutcome {
	var outcomes []plugin.AttemptTestOutcome
	for _, spec := range suite.Specs {
		var status string
		if !spec.Ok {
			status = plugin.TestStatusFailed
		} else if len(spec.Tests) > 0 && spec.Tests[0].Status == "skipped" {
			status = plugin.TestStatusSkipped
		} else {
			status = plugin.TestStatusPassed
		}
		if spec.Ok {
			for _, run := range spec.Tests {
				if run.Status == "skipped" && run.ExpectedStatus != "skipped" {
					status = plugin.TestStatusNotRun
				}
			}
		}
		outcomes = append(outcomes, plugin.AttemptTestOutcome{
			TestCase: mapSpecToTestCase(spec, suiteName),
			Status:   status,
		})
	}
	for _, subSuite := range suite.Suites {
		outcomes = append(outcomes, getTestResultsFromSuite(subSuite, fmt.Sprintf("%s %s", suiteName, subSuite.Title))...)
	}
	return outcomes
}

func getTestCasesFromSuite(suite playwrightReportSuite, suiteName string) []plugin.TestCase {
	var cases []plugin.TestCase
	for _, spec := range suite.Specs {
		cases = append(cases, mapSpecToTestCase(spec, suiteName))
	}
	for _, subSuite := range suite.Suites {
		cases = append(cases, getTestCasesFromSuite(subSuite, fmt.Sprintf("%s %s", suiteName, subSuite.Title))...)
	}
	return cases
}

func mapSpecToTestCase(spec playwrightSpec, suiteName string) plugin.TestCase {
	projectName := ""
	if len(spec.Tests) > 0 {
		projectName = spec.Tests[0].ProjectName
	}
	return plugin.TestCase{
		Name:  spec.Title,
		Path:  fmt.Sprintf("%s:%d", spec.File, spec.Line),
		Scope: fmt.Sprintf(" %s %s %s", projectName, suiteName, spec.Title),
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

	return words[0], words[1:], nil
}
