// Command bktest-runner-jest is the Go runner plugin for Jest.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
)

type jest struct {
	cfg runnerplugin.Config
	log runnerplugin.Logger
}

func main() { runnerplugin.Serve(&jest{}) }

func (j *jest) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	j.cfg = params.Config
	j.log.Enabled = j.cfg.Debug
	if j.cfg.TestCommand == "" {
		j.cfg.TestCommand = "npx jest --runTestsByPath {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}"
	}
	if j.cfg.TestFilePattern == "" {
		j.cfg.TestFilePattern = "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}"
	}
	if j.cfg.RetryTestCommand == "" {
		j.cfg.RetryTestCommand = "npx jest --testNamePattern '{{testNamePattern}}' --json --testLocationInResults --outputFile {{resultPath}}"
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "Jest",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			FilterTestFiles: true,
			AutoRetry:       true,
			Mute:            true,
			SplitBySelector: true,
		},
		ResultFormat:   "jest-json",
		ResultPath:     j.cfg.ResultPath,
		LocationPrefix: j.cfg.LocationPrefix,
	}, nil
}

func (j *jest) Discover() (runnerplugin.DiscoverResult, error) {
	j.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", j.cfg.TestFilePattern, j.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(j.cfg.TestFilePattern, j.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

func (j *jest) Examples(runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	return runnerplugin.ExamplesResult{}, errors.New("jest does not support split by example")
}

// Command runs files on the first attempt, and retries by an anchored name
// pattern built from the failed tests' scope and name.
func (j *jest) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	cmd := j.cfg.TestCommand
	if params.Retry {
		cmd = j.cfg.RetryTestCommand
	}
	words, err := shellquote.Split(cmd)
	if err != nil {
		return runnerplugin.CommandResult{}, err
	}
	paths := runnerplugin.Paths(params.TestCases)
	slices.Sort(paths)
	paths = slices.Compact(paths)

	if params.Retry {
		idx := slices.Index(words, "{{testNamePattern}}")
		if idx < 0 {
			return runnerplugin.CommandResult{}, fmt.Errorf("couldn't find '{{testNamePattern}}' sentinel in retry command")
		}
		// Jest matches the pattern anywhere in the full name, which joins the
		// describe titles and the title with spaces. Anchor it so only the
		// failed tests run.
		names := make([]string, len(params.TestCases))
		for i, tc := range params.TestCases {
			fullName := tc.Name
			if tc.Scope != "" {
				fullName = tc.Scope + " " + tc.Name
			}
			names[i] = regexp.QuoteMeta(fullName)
		}
		words = slices.Replace(words, idx, idx+1, fmt.Sprintf("^(%s)$", strings.Join(names, "|")))
		if idx := slices.Index(words, "{{testExamples}}"); idx >= 0 {
			words = slices.Replace(words, idx, idx+1, paths...)
		}
	} else if idx := slices.Index(words, "{{testExamples}}"); idx < 0 {
		words = append(words, paths...)
	} else {
		words = slices.Replace(words, idx, idx+1, paths...)
	}

	idx := slices.Index(words, "{{resultPath}}")
	if idx < 0 {
		return runnerplugin.CommandResult{}, fmt.Errorf("couldn't find '{{resultPath}}' sentinel in command, exiting")
	}
	words = slices.Replace(words, idx, idx+1, j.cfg.ResultPath)
	return runnerplugin.CommandResult{Argv: words}, nil
}

type assertion struct {
	Status         string   `json:"status"`
	Title          string   `json:"title"`
	AncestorTitles []string `json:"ancestorTitles"`
}

type report struct {
	TestResults []struct {
		AssertionResults []assertion `json:"assertionResults"`
		FileName         string      `json:"name"`
		Status           string      `json:"status"`
	} `json:"testResults"`
}

func (j *jest) Interpret(params runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	var result runnerplugin.InterpretResult
	var r report
	data, err := os.ReadFile(j.cfg.ResultPath)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	if err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Jest output, tests will not be retried: %v", err))
		return result, nil
	}

	workDir, err := os.Getwd()
	if err != nil {
		return result, fmt.Errorf("failed to get current working directory: %v", err)
	}
	for _, file := range r.TestResults {
		path, err := filepath.Rel(workDir, file.FileName)
		if err != nil {
			return result, fmt.Errorf("failed to get relative path of test file: %v", err)
		}
		// A failed file without tests had a runtime error, such as one while
		// loading. Jest still ran the other files, so their failures retry.
		if file.Status == "failed" && len(file.AssertionResults) == 0 {
			result.ErroredFiles = append(result.ErroredFiles, path)
		}
		for _, a := range file.AssertionResults {
			status := runnerplugin.StatusUnknown
			switch a.Status {
			case "failed":
				status = runnerplugin.StatusFailed
			case "passed":
				status = runnerplugin.StatusPassed
			case "pending", "todo":
				status = runnerplugin.StatusSkipped
			}
			// Retries run by name, and Jest reports the tests the pattern
			// doesn't match as pending. Those keep their earlier result.
			if params.Retry && status == runnerplugin.StatusSkipped {
				continue
			}
			// Matches the Buildkite JavaScript test collector's identity.
			result.Tests = append(result.Tests, runnerplugin.TestResult{
				TestCase: runnerplugin.TestCase{Name: a.Title, Scope: strings.Join(a.AncestorTitles, " "), Path: path},
				Status:   status,
			})
		}
	}
	return result, nil
}
