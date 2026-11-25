// Command bktest-runner-playwright is the Go runner plugin for Playwright.
package main

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

type playwright struct {
	cfg runnerplugin.Config
	log runnerplugin.Logger
}

func main() { runnerplugin.Serve(&playwright{}) }

func (p *playwright) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	p.cfg = params.Config
	p.log.Enabled = p.cfg.Debug
	if p.cfg.TestCommand == "" {
		p.cfg.TestCommand = "npx playwright test"
	}
	if p.cfg.RetryTestCommand == "" {
		p.cfg.RetryTestCommand = p.cfg.TestCommand
	}
	if p.cfg.TestFilePattern == "" {
		p.cfg.TestFilePattern = "**/{*.spec,*.test}.{ts,js}"
	}
	resultFormat := ""
	if p.cfg.ResultPath != "" {
		resultFormat = "playwright-json"
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "Playwright",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			AutoRetry:       true,
			Mute:            true,
			// A file:line runs in every project, so skipping one project's
			// example would still run the others.
			Skip:            false,
			SplitBySelector: true,
		},
		ResultFormat:   resultFormat,
		ResultPath:     p.cfg.ResultPath,
		LocationPrefix: p.cfg.LocationPrefix,
	}, nil
}

func (p *playwright) Discover() (runnerplugin.DiscoverResult, error) {
	p.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", p.cfg.TestFilePattern, p.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(p.cfg.TestFilePattern, p.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

func (p *playwright) argv(testCases []runnerplugin.TestCase, retry bool) ([]string, error) {
	cmd := p.cfg.TestCommand
	if retry {
		cmd = p.cfg.RetryTestCommand
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
	return words, nil
}

func (p *playwright) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	argv, err := p.argv(params.TestCases, params.Retry)
	return runnerplugin.CommandResult{Argv: argv}, err
}

// Examples lists tests with `--list --reporter=json`. The JSON reporter's
// output file can only be set with an environment variable.
func (p *playwright) Examples(params runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	if len(params.Files) == 0 {
		return runnerplugin.ExamplesResult{TestCases: []runnerplugin.TestCase{}}, nil
	}
	tmp, err := os.CreateTemp("", "playwright-list-*.json")
	if err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	argv, err := p.argv(runnerplugin.TestCasesFromPaths(params.Files), false)
	if err != nil {
		return runnerplugin.ExamplesResult{}, err
	}
	argv = append(argv, "--list", "--reporter=json")
	p.log.Printf("Running `%s` to list tests", strings.Join(argv, " "))
	// #nosec G204 -- the test command is configured by the bktest user.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_JSON_OUTPUT_NAME="+tmp.Name())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to list playwright tests: %w\nstderr: %s", err, stderr.String())
	}
	r, err := readReport(tmp.Name())
	if err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to parse playwright test list output: %w", err)
	}
	var result runnerplugin.ExamplesResult
	walk(r.Suites, func(spec spec, suiteName string) {
		result.TestCases = append(result.TestCases, spec.testCase(suiteName))
	})
	return result, nil
}

func (p *playwright) Interpret(params runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	var result runnerplugin.InterpretResult
	r, err := readReport(p.cfg.ResultPath)
	if err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Playwright output, tests will not be retried: %v", err))
		return result, nil
	}

	walk(r.Suites, func(spec spec, suiteName string) {
		status := runnerplugin.StatusPassed
		switch {
		case !spec.Ok:
			status = runnerplugin.StatusFailed
		case spec.Tests[0].Status == "skipped":
			status = runnerplugin.StatusSkipped
		}
		if spec.Ok {
			for _, run := range spec.Tests {
				// A test skipped because something else failed didn't run, and
				// a passing retry must not hide that.
				if run.Status == "skipped" && run.ExpectedStatus != "skipped" {
					status = runnerplugin.StatusNotRun
				}
			}
		}
		result.Tests = append(result.Tests, runnerplugin.TestResult{TestCase: spec.testCase(suiteName), Status: status})
	})

	if len(r.Errors) == 0 {
		return result, nil
	}

	// Playwright runs no tests when a test file fails to load, and locates
	// the error in that file. Errors elsewhere, such as in a global setup,
	// error the whole run.
	files, ok := errorFiles(r.Errors)
	unmatched := slices.Clone(files)
	var remaining []runnerplugin.TestCase
	for _, tc := range params.TestCases {
		file := tc.Path
		if tc.Format == runnerplugin.FormatSelector {
			file = tc.Value
		}
		file = strings.TrimPrefix(lineSuffix.ReplaceAllString(file, ""), "./")
		if slices.Contains(files, file) {
			unmatched = slices.DeleteFunc(unmatched, func(f string) bool { return f == file })
		} else {
			remaining = append(remaining, tc)
		}
	}
	if !ok || len(r.Suites) > 0 || len(unmatched) > 0 {
		result.Error = "Playwright failed with errors"
		return result, nil
	}
	result.ErroredFiles = files
	if len(remaining) > 0 {
		result.Messages = append(result.Messages, fmt.Sprintf("+++ Buildkite Test Engine Client: Playwright failed to load %s. Running the other test files again.", strings.Join(files, ", ")))
		result.Rerun = remaining
	}
	return result, nil
}

var lineSuffix = regexp.MustCompile(`:\d+$`)

// errorFiles returns the files, relative to the working directory, that the
// errors are located in. It returns false if any error has no location.
func errorFiles(errors []reportError) ([]string, bool) {
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

type test struct {
	ProjectName    string `json:"projectName"`
	Status         string `json:"status"`
	ExpectedStatus string `json:"expectedStatus"`
}

type spec struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	Title string `json:"title"`
	Ok    bool   `json:"ok"`
	Tests []test `json:"tests"`
}

type suite struct {
	Title  string  `json:"title"`
	Specs  []spec  `json:"specs"`
	Suites []suite `json:"suites"`
}

type reportError struct {
	Location *struct {
		File string `json:"file"`
	} `json:"location"`
}

type report struct {
	Suites []suite       `json:"suites"`
	Errors []reportError `json:"errors"`
}

func readReport(path string) (report, error) {
	var r report
	data, err := os.ReadFile(path)
	if err != nil {
		return r, fmt.Errorf("failed to read playwright output: %v", err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("failed to parse playwright output: %s", err)
	}
	return r, nil
}

// walk visits every spec with its suite titles joined by spaces.
func walk(suites []suite, visit func(spec, string)) {
	var rec func(s suite, name string)
	rec = func(s suite, name string) {
		for _, sp := range s.Specs {
			visit(sp, name)
		}
		for _, child := range s.Suites {
			rec(child, name+" "+child.Title)
		}
	}
	for _, s := range suites {
		rec(s, s.Title)
	}
}

// testCase matches the Buildkite Playwright collector, whose scope is the
// title path: the project, the suites and the test title.
func (s spec) testCase(suiteName string) runnerplugin.TestCase {
	return runnerplugin.TestCase{
		Name:  s.Title,
		Path:  fmt.Sprintf("%s:%d", s.File, s.Line),
		Scope: fmt.Sprintf(" %s %s %s", s.Tests[0].ProjectName, suiteName, s.Title),
	}
}
