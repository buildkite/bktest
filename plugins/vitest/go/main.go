// Command bktest-runner-vitest is the Go runner plugin for Vitest.
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

const failedTestsMessage = "Vitest finished with failed tests; see the report above"

type vitest struct {
	cfg runnerplugin.Config
	log runnerplugin.Logger
	// retryNames keeps the exact names learned from earlier reports, keyed
	// by test identity, because suite boundaries can't be recovered from
	// the scope. The plugin process lives for the whole bktest run.
	retryNames map[string][]string
}

func main() { runnerplugin.Serve(&vitest{retryNames: map[string][]string{}}) }

func (v *vitest) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	v.cfg = params.Config
	v.log.Enabled = v.cfg.Debug
	if v.cfg.TestCommand == "" {
		v.cfg.TestCommand = "npx vitest run {{testExamples}} --reporter=default --reporter=json --outputFile {{resultPath}}"
	}
	if v.cfg.TestFilePattern == "" {
		v.cfg.TestFilePattern = "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}"
	}
	if v.cfg.RetryTestCommand == "" {
		v.cfg.RetryTestCommand = "npx vitest run --testNamePattern '{{testNamePattern}}' --reporter=default --reporter=json --outputFile {{resultPath}}"
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "Vitest",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			FilterTestFiles: true,
			AutoRetry:       true,
			Mute:            true,
			SplitBySelector: true,
		},
		ResultFormat:   "jest-json",
		ResultPath:     v.cfg.ResultPath,
		LocationPrefix: v.cfg.LocationPrefix,
	}, nil
}

func (v *vitest) Discover() (runnerplugin.DiscoverResult, error) {
	v.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", v.cfg.TestFilePattern, v.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(v.cfg.TestFilePattern, v.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

func (v *vitest) Examples(runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	return runnerplugin.ExamplesResult{}, errors.New("vitest does not support split by example")
}

func identity(tc runnerplugin.TestCase) string {
	return tc.Scope + "/" + tc.Name + "/" + tc.Path
}

func (v *vitest) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	cmd := v.cfg.TestCommand
	if params.Retry {
		cmd = v.cfg.RetryTestCommand
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
		var escaped []string
		for _, tc := range params.TestCases {
			names := v.retryNames[identity(tc)]
			if len(names) == 0 {
				fullName := tc.Name
				if tc.Scope != "" {
					fullName = tc.Scope + " " + tc.Name
				}
				names = []string{fullName}
			}
			for _, name := range names {
				escaped = append(escaped, regexp.QuoteMeta(name))
			}
		}
		// Match whole names, not tests whose names contain a failed name.
		words = slices.Replace(words, idx, idx+1, fmt.Sprintf("^(%s)$", strings.Join(escaped, "|")))
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
	words = slices.Replace(words, idx, idx+1, v.cfg.ResultPath)
	return runnerplugin.CommandResult{Argv: words}, nil
}

type report struct {
	TestResults []struct {
		AssertionResults []struct {
			Status         string   `json:"status"`
			Title          string   `json:"title"`
			AncestorTitles []string `json:"ancestorTitles"`
		} `json:"assertionResults"`
		FileName string `json:"name"`
		Status   string `json:"status"`
	} `json:"testResults"`
}

func (v *vitest) Interpret(params runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	// Vitest can exit 0 with failed tests, so bktest fails the attempt then.
	result := runnerplugin.InterpretResult{FailedTestsMessage: failedTestsMessage}
	var r report
	data, err := os.ReadFile(v.cfg.ResultPath)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	if err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Vitest output, tests will not be retried: %v", err))
		return result, nil
	}

	workDir, err := os.Getwd()
	if err != nil {
		return result, fmt.Errorf("failed to get current working directory: %v", err)
	}
	for _, file := range r.TestResults {
		// A failed file without tests had a runtime error, such as an import
		// that throws.
		if file.Status == "failed" && len(file.AssertionResults) == 0 {
			result.Error = "Vitest failed with runtime error test suites"
		}
		for _, a := range file.AssertionResults {
			status := runnerplugin.StatusUnknown
			switch a.Status {
			case "failed":
				status = runnerplugin.StatusFailed
			case "passed":
				status = runnerplugin.StatusPassed
			// Vitest reports it.skip and it.todo as "skipped"; Jest as "pending".
			case "pending", "skipped", "todo":
				status = runnerplugin.StatusSkipped
			}
			path, err := filepath.Rel(workDir, file.FileName)
			if err != nil {
				return result, fmt.Errorf("failed to get relative path of test file: %v", err)
			}
			tc := runnerplugin.TestCase{Name: a.Title, Scope: strings.Join(a.AncestorTitles, " "), Path: path}
			// A skipped test on a name-filtered retry didn't run.
			if params.Retry && status == runnerplugin.StatusSkipped {
				continue
			}
			// Vitest 4 matches space-separated names; Vitest 5 uses " > ".
			titles := append(slices.Clone(a.AncestorTitles), a.Title)
			v.retryNames[identity(tc)] = slices.Compact([]string{
				strings.Join(titles, " "),
				strings.Join(titles, " > "),
			})
			result.Tests = append(result.Tests, runnerplugin.TestResult{TestCase: tc, Status: status})
		}
	}
	return result, nil
}
