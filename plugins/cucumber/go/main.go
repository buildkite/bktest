// Command bktest-runner-cucumber is the Go runner plugin for Cucumber (Ruby).
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

type cucumber struct {
	cfg runnerplugin.Config
	log runnerplugin.Logger
}

func main() { runnerplugin.Serve(&cucumber{}) }

func (c *cucumber) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	c.cfg = params.Config
	c.log.Enabled = c.cfg.Debug
	if c.cfg.TestCommand == "" {
		c.cfg.TestCommand = "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}"
	}
	if c.cfg.TestFilePattern == "" {
		c.cfg.TestFilePattern = "features/**/*.feature"
	}
	if c.cfg.RetryTestCommand == "" {
		c.cfg.RetryTestCommand = c.cfg.TestCommand
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "Cucumber",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
			SplitBySelector: true,
		},
		ResultFormat:   "cucumber-json",
		ResultPath:     c.cfg.ResultPath,
		LocationPrefix: c.cfg.LocationPrefix,
	}, nil
}

func (c *cucumber) Discover() (runnerplugin.DiscoverResult, error) {
	c.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", c.cfg.TestFilePattern, c.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(c.cfg.TestFilePattern, c.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

func (c *cucumber) argv(testCases []runnerplugin.TestCase, retry bool) ([]string, error) {
	cmd := c.cfg.TestCommand
	if retry {
		cmd = c.cfg.RetryTestCommand
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
		words = slices.Replace(words, idx, idx+1, c.cfg.ResultPath)
	}
	return words, nil
}

func (c *cucumber) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	argv, err := c.argv(params.TestCases, params.Retry)
	return runnerplugin.CommandResult{Argv: argv}, err
}

// Examples dry-runs Cucumber to list scenarios as feature file:line.
func (c *cucumber) Examples(params runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	if len(params.Files) == 0 {
		return runnerplugin.ExamplesResult{TestCases: []runnerplugin.TestCase{}}, nil
	}
	f, err := os.CreateTemp("", "cucumber-dry-run-*.json")
	if err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to create temporary file for cucumber dry run: %w", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	argv, err := c.argv(runnerplugin.TestCasesFromPaths(params.Files), false)
	if err != nil {
		return runnerplugin.ExamplesResult{}, err
	}
	args := append([]string{"--dry-run", "--format", "json", "--out", f.Name(), "--format", "progress"}, params.Files...)
	c.log.Printf("Running `%s %s` for dry run", argv[0], strings.Join(args, " "))
	// #nosec G204 -- the test command is configured by the bktest user.
	if output, err := exec.Command(argv[0], args...).CombinedOutput(); err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to run Cucumber dry run: %s", output)
	}
	features, err := readReport(f.Name(), true)
	if err != nil {
		return runnerplugin.ExamplesResult{}, fmt.Errorf("failed to parse cucumber dry run JSON report from %s: %w", f.Name(), err)
	}
	var result runnerplugin.ExamplesResult
	for _, feature := range features {
		for _, scenario := range feature.Elements {
			if scenario.Type == "scenario" {
				result.TestCases = append(result.TestCases, scenarioTestCase(feature, scenario))
			}
		}
	}
	return result, nil
}

func (c *cucumber) Interpret(runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	var result runnerplugin.InterpretResult
	features, err := readReport(c.cfg.ResultPath, false)
	if err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: %v", err))
		return result, nil
	}
	for _, feature := range features {
		for _, scenario := range feature.Elements {
			if scenario.Type != "scenario" {
				continue
			}
			status := runnerplugin.StatusUnknown
			switch scenario.aggregatedStatus() {
			case "failed":
				status = runnerplugin.StatusFailed
			case "passed":
				status = runnerplugin.StatusPassed
			case "pending":
				status = runnerplugin.StatusSkipped
			}
			result.Tests = append(result.Tests, runnerplugin.TestResult{TestCase: scenarioTestCase(feature, scenario), Status: status})
		}
	}
	return result, nil
}

type feature struct {
	URI      string    `json:"uri"`
	Name     string    `json:"name"`
	Elements []element `json:"elements"`
}

type element struct {
	Name  string `json:"name"`
	Line  int    `json:"line"`
	Type  string `json:"type"`
	Steps []struct {
		Result *struct {
			Status string `json:"status"`
		} `json:"result"`
	} `json:"steps"`
}

// aggregatedStatus is "failed" if any step failed or is undefined,
// "pending" if any step is pending or skipped, and otherwise "passed".
func (e element) aggregatedStatus() string {
	status := "passed"
	for _, step := range e.Steps {
		if step.Result == nil {
			continue
		}
		switch step.Result.Status {
		case "failed", "undefined", "errored":
			return "failed"
		case "pending", "skipped":
			status = "pending"
		}
	}
	return status
}

// scenarioTestCase identifies a scenario by its feature file and line.
func scenarioTestCase(f feature, e element) runnerplugin.TestCase {
	location := fmt.Sprintf("%s:%d", f.URI, e.Line)
	return runnerplugin.TestCase{Identifier: location, Name: e.Name, Scope: f.Name, Path: location}
}

// readReport reads Cucumber JSON. A dry run's report can be empty, or a
// single feature object instead of a list.
func readReport(path string, dryRun bool) ([]feature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cucumber output: %v", err)
	}
	var features []feature
	if dryRun {
		if len(data) == 0 {
			return []feature{}, nil
		}
		if err := json.Unmarshal(data, &features); err != nil {
			var single feature
			if singleErr := json.Unmarshal(data, &single); singleErr != nil {
				return nil, fmt.Errorf("failed to unmarshal cucumber json report from %s: %w. Single feature unmarshal error: %v", path, err, singleErr)
			}
			features = []feature{single}
		}
		return features, nil
	}
	if err := json.Unmarshal(data, &features); err != nil {
		return nil, fmt.Errorf("failed to parse cucumber output: %s", err)
	}
	return features, nil
}
