// Package playwright is the runner plugin for Playwright.
package playwright

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
)

type handler struct{}

// New returns the Playwright handler.
func New() runnerplugin.Handler { return handler{} }

var (
	_ runnerplugin.ExampleDiscoverer  = handler{}
	_ runnerplugin.AttemptInterpreter = handler{}
	_ runnerplugin.BatchReportParser  = handler{}
)

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	if c.TestCommand == "" {
		c.TestCommand = "npx playwright test"
	}

	if c.RetryTestCommand == "" {
		c.RetryTestCommand = c.TestCommand
	}

	if c.TestFilePattern == "" {
		c.TestFilePattern = "**/{*.spec,*.test}.{ts,js}"
	}

	resultFormat := "playwright-json"
	if c.ResultPath == "" {
		resultFormat = ""
	}

	return runnerplugin.ConfigureResponse{
		Name:   "Playwright",
		Config: c,
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       true,
			Mute:            true,
			// Skip stays disabled for now. The same test can run across multiple
			// Playwright projects (e.g. Chromium and Firefox) as separate examples
			// that share the same Path (file:line). The value we hand to Playwright
			// is just that Path, which is not project-specific, so skipping one
			// project's example while keeping another's would still rerun both.
			// Enabling skip needs a project-scoped selector first.
			Skip:            false,
			SplitBySelector: true,
		},
		ResultFormat: resultFormat,
	}, nil
}

// InterpretAttempt records the tests in Playwright's JSON report. When test
// files failed to load, it asks bktest to run the other files again.
func (handler) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error) {
	var response runnerplugin.InterpretAttemptResponse

	// Playwright exits with a non-zero status code when there are test failures,
	// so we should always attempt to parse the report even if the command returns an error.
	report, parseErr := ReadReport(request.Config.ResultPath)
	if parseErr != nil {
		// We don't want to fail the build if we fail to parse the report:
		// the attempt's error stays the command error (which can be nil), instead of the parse error.
		response.Messages = append(response.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: %v", parseErr))
		return response, nil
	}

	for _, suite := range report.Suites {
		response.Tests = append(response.Tests, getTestResultsFromSuite(suite, suite.Title)...)
	}

	if len(report.Errors) == 0 {
		return response, nil
	}

	// Playwright runs no tests at all when a test file fails to load.
	files, ok := errorFiles(report.Errors)
	unmatched := slices.Clone(files)
	var remaining []runnerplugin.TestCase
	for _, testCase := range request.TestCases {
		file := testCase.Path
		if testCase.Format == runnerplugin.FormatSelector {
			file = testCase.Value
		}
		file = strings.TrimPrefix(lineSuffix.ReplaceAllString(file, ""), "./")
		if slices.Contains(files, file) {
			unmatched = slices.DeleteFunc(unmatched, func(f string) bool { return f == file })
		} else {
			remaining = append(remaining, testCase)
		}
	}
	// Only errors in the files given to the command are attributed to them.
	// Others, such as an error in a global setup, error the whole run.
	if !ok || len(report.Suites) > 0 || len(unmatched) > 0 {
		response.RunError = "Playwright failed with errors"
		return response, nil
	}

	response.ErroredFiles = files
	if len(remaining) == 0 {
		return response, nil
	}
	response.Messages = append(response.Messages, fmt.Sprintf("+++ Buildkite Test Engine Client: Playwright failed to load %s. Running the other test files again.", strings.Join(files, ", ")))
	// The command failed for the files that didn't load, even if the rerun passes.
	response.Rerun = remaining
	return response, nil
}

// lineSuffix matches the line of a file:line test location.
var lineSuffix = regexp.MustCompile(`:\d+$`)

// errorFiles returns the files, relative to the working
// directory, that Playwright's errors are located in, such as test files that
// failed to load. It returns false if any error has no location.
func errorFiles(errors []Error) ([]string, bool) {
	workDir, err := os.Getwd()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, e := range errors {
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

// getTestResultsFromSuite recursively collects test outcomes from the report.
func getTestResultsFromSuite(suite ReportSuite, suiteName string) []runnerplugin.TestResult {
	var testResults []runnerplugin.TestResult

	for _, spec := range suite.Specs {
		var status string
		if !spec.Ok {
			status = runnerplugin.StatusFailed
		} else if spec.Tests[0].Status == "skipped" {
			status = runnerplugin.StatusSkipped
		} else {
			status = runnerplugin.StatusPassed
		}
		if spec.Ok {
			for _, run := range spec.Tests {
				// Failure-induced skips must not be hidden by a passing retry.
				if run.Status == "skipped" && run.ExpectedStatus != "skipped" {
					status = runnerplugin.StatusNotRun
				}
			}
		}

		testResults = append(testResults, runnerplugin.TestResult{
			TestCase: mapSpecToTestCase(spec, suiteName),
			Status:   status,
		})
	}

	for _, subSuite := range suite.Suites {
		testResults = append(testResults, getTestResultsFromSuite(subSuite, fmt.Sprintf("%s %s", suiteName, subSuite.Title))...)
	}

	return testResults
}

func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	words, err := commandWords(request.Config, request.TestCases, request.Retry)
	if err != nil {
		return runnerplugin.PrepareAttemptResponse{}, err
	}
	return runnerplugin.PrepareAttemptResponse{Argv: words}, nil
}

// commandWords replaces the "{{testExamples}}" placeholder in the test
// command with the test cases, or appends them when there is none.
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

	idx := slices.Index(words, "{{testExamples}}")
	if idx < 0 {
		words = append(words, testPaths...)
	} else {
		words = slices.Replace(words, idx, idx+1, testPaths...)
	}

	return words, nil
}

// ParseBatchReport handles the JSON report of one Playwright command
// run by bktec pool consume. A test runs once in each project and is reported
// once per project, with the project in its scope like Buildkite Test
// Collector, so muting stays per project while the attempt of the test's
// leased file accounts for every project.
func (handler) ParseBatchReport(request runnerplugin.ParseBatchReportRequest) (runnerplugin.ParseBatchReportResponse, error) {
	if request.Format != "playwright-json" {
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("unsupported runner report format %q", request.Format)
	}
	data, err := os.ReadFile(request.ReportPath)
	if err != nil {
		return runnerplugin.ParseBatchReportResponse{}, err
	}
	var report struct {
		Config struct {
			RootDir string `json:"rootDir"`
		} `json:"config"`
		Suites *[]ReportSuite `json:"suites"`
		Errors []Error        `json:"errors"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("invalid Playwright report: %w", err)
	}
	if report.Suites == nil || report.Config.RootDir == "" {
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("incomplete Playwright report: missing suites or config.rootDir")
	}
	workDir, err := os.Getwd()
	if err != nil {
		return runnerplugin.ParseBatchReportResponse{}, err
	}
	parsed := runnerplugin.ParseBatchReportResponse{
		// Playwright runs the tests of setup projects with every command.
		UnselectedTests: true,
	}
	seen := map[string]int{}
	var walk func(suite ReportSuite, titles string) error
	walk = func(suite ReportSuite, titles string) error {
		for _, spec := range suite.Specs {
			if spec.File == "" {
				return fmt.Errorf("playwright test %q has no file", spec.Title)
			}
			// Spec files are relative to the root directory; selectors to the working directory.
			path, err := filepath.Rel(workDir, filepath.Join(report.Config.RootDir, spec.File))
			if err != nil {
				return fmt.Errorf("failed to get relative path of test file: %w", err)
			}
			path = filepath.ToSlash(path)
			for _, run := range spec.Tests {
				var status string
				switch run.Status {
				case "expected", "flaky":
					status = runnerplugin.StatusPassed
				case "unexpected":
					status = runnerplugin.StatusFailed
				case "skipped":
					status = runnerplugin.StatusSkipped
					// A test that wasn't meant to be skipped didn't run because
					// something else failed, such as an earlier test in a serial
					// group. A passing retry of that test must not hide it.
					if run.ExpectedStatus != "skipped" && !slices.Contains(parsed.ErroredFiles, path) {
						parsed.ErroredFiles = append(parsed.ErroredFiles, path)
					}
				default:
					return fmt.Errorf("unknown Playwright test status %q", run.Status)
				}
				// Matches the scope of Buildkite Test Collector, as in mapSpecToTestCase.
				test := runnerplugin.BatchTest{
					TestCase: runnerplugin.TestCase{
						Scope: fmt.Sprintf(" %s %s %s", run.ProjectName, titles, spec.Title),
						Name:  spec.Title,
						Path:  fmt.Sprintf("%s:%d", path, spec.Line),
					},
					Status:   status,
					Selector: path,
				}
				// A test repeated in one project, such as with --repeat-each, is
				// reported once: failed if any run failed.
				key := strings.Join([]string{path, test.TestCase.Scope}, "\x00")
				if i, ok := seen[key]; ok {
					if status == runnerplugin.StatusFailed || parsed.Tests[i].Status == runnerplugin.StatusSkipped {
						parsed.Tests[i].Status = status
					}
					continue
				}
				seen[key] = len(parsed.Tests)
				parsed.Tests = append(parsed.Tests, test)
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
			return runnerplugin.ParseBatchReportResponse{}, err
		}
	}
	if len(report.Errors) > 0 {
		// Playwright runs no tests at all when a test file fails to load, and
		// locates the error in that file. Other errors outside tests don't say
		// which tests they affect.
		files, ok := errorFiles(report.Errors)
		if ok && len(parsed.Tests) == 0 {
			parsed.ErroredFiles = files
			parsed.ErroredFilesStoppedRun = true
		} else {
			parsed.ErrorsOutsideTests = true
		}
	}
	return parsed, nil
}

// ReadReport reads the Playwright JSON report at path.
func ReadReport(path string) (Report, error) {
	var report Report
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("failed to read playwright output: %v", err)
	}

	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, fmt.Errorf("failed to parse playwright output: %s", err)
	}

	return report, nil
}

// DiscoverExamples returns the test examples within the given files.
func (handler) DiscoverExamples(request runnerplugin.DiscoverExamplesRequest) (runnerplugin.DiscoverExamplesResponse, error) {
	var response runnerplugin.DiscoverExamplesResponse
	if len(request.Files) == 0 {
		return response, nil
	}

	// Create a temporary file for JSON output
	// This approach works with both npm/npx and yarn, avoiding yarn's stdout prefix issues
	tmpFile, err := os.CreateTemp("", "playwright-list-*.json")
	if err != nil {
		return response, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	words, err := commandWords(request.Config, runnerplugin.TestCasesFromPaths(request.Files), false)
	if err != nil {
		return response, err
	}

	words = append(words, "--list", "--reporter=json")

	request.Config.Debugf("Running `%s` to list tests", strings.Join(words, " "))

	// #nosec G204 -- the test command is configured by the bktest user.
	cmd := exec.Command(words[0], words[1:]...)
	// Playwright's JSON reporter output path can only be set via env var, not CLI args.
	// See: https://playwright.dev/docs/test-reporters#json-reporter
	cmd.Env = append(os.Environ(), fmt.Sprintf("PLAYWRIGHT_JSON_OUTPUT_NAME=%s", tmpPath))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return response, fmt.Errorf("failed to list playwright tests: %w\nstderr: %s", err, stderr.String())
	}

	report, err := ReadReport(tmpPath)
	if err != nil {
		return response, fmt.Errorf("failed to parse playwright test list output: %w", err)
	}

	for _, suite := range report.Suites {
		response.TestCases = append(response.TestCases, getTestCasesFromSuite(suite, suite.Title)...)
	}

	return response, nil
}

// getTestCasesFromSuite recursively traverses the Playwright report suite and returns all test cases.
// Playwright's report format is a tree structure, where each suite can contain multiple specs and sub-suites.
func getTestCasesFromSuite(suite ReportSuite, suiteName string) []runnerplugin.TestCase {
	var testCases []runnerplugin.TestCase

	for _, spec := range suite.Specs {
		testCases = append(testCases, mapSpecToTestCase(spec, suiteName))
	}

	for _, subSuite := range suite.Suites {
		testCases = append(testCases, getTestCasesFromSuite(subSuite, fmt.Sprintf("%s %s", suiteName, subSuite.Title))...)
	}

	return testCases
}

// mapSpecToTestCase converts a Playwright spec to a test case.
// The scope has to match with the scope generated by Buildkite test collector.
// In Buildkite test collector, the scope is generated using Playwright built-in reporter function, titlePath().
// titlePath function returns an array of suite's title from the root suite down to the current test,
// which is then joined with a space separator to form the scope.
// For more details, see:
// [Buildkite Test Collector - Playwright implementation](https://github.com/buildkite/test-collector-javascript/blob/42b803a618a15a07edf0169038ef4b5eba88f98d/playwright/reporter.js#L47)
// [Playwright titlePath implementation](https://github.com/microsoft/playwright/blob/523e50088a7f982dd96aacdb260dfbd1189159b1/packages/playwright/src/common/test.ts#L126)
// [Playwright suite structure](https://playwright.dev/docs/api/class-suite)
func mapSpecToTestCase(spec Spec, suiteName string) runnerplugin.TestCase {
	projectName := spec.Tests[0].ProjectName
	return runnerplugin.TestCase{
		Name:  spec.Title,
		Path:  fmt.Sprintf("%s:%d", spec.File, spec.Line),
		Scope: fmt.Sprintf(" %s %s %s", projectName, suiteName, spec.Title),
	}
}

// Test is a run of a spec in one Playwright project.
type Test struct {
	ProjectName    string
	Status         string
	ExpectedStatus string
}

// Spec is a test in a Playwright JSON report.
type Spec struct {
	File   string
	Line   int
	Column int
	ID     string
	Title  string
	Ok     bool
	Tests  []Test
}

// ReportSuite is a file or describe block in a Playwright JSON report.
type ReportSuite struct {
	Title  string
	Specs  []Spec
	Suites []ReportSuite
}

// Report is the structure of Playwright's JSON report.
type Report struct {
	Suites []ReportSuite
	Stats  struct {
		Expected   int
		Unexpected int
	}
	Errors []Error
}

// Error is an error outside the tests in a Playwright JSON report.
type Error struct {
	Message  string
	Location *struct {
		File string
	}
}
