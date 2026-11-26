// Package custom is the runner plugin for test runners bktest has no
// integration for, which run with a command the user provides.
package custom

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
)

type handler struct{}

// New returns the custom runner handler.
func New() runnerplugin.Handler { return handler{} }

var (
	_ runnerplugin.TargetDiscoverer   = handler{}
	_ runnerplugin.AttemptInterpreter = handler{}
)

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	if c.TestCommand == "" {
		return runnerplugin.ConfigureResponse{}, errors.New("test command must be provided for custom runner")
	}

	// When splitting work using a user-provided selector list, test file
	// discovery is skipped, so the test file pattern isn't required.
	if c.TestFilePattern == "" && c.SelectorListPath == "" {
		return runnerplugin.ConfigureResponse{}, errors.New("test file pattern must be provided for custom runner")
	}

	if c.RetryTestCommand == "" {
		c.RetryTestCommand = c.TestCommand
	}

	return runnerplugin.ConfigureResponse{
		Name:   "Custom test runner",
		Config: c,
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  false,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       false,
			Mute:            true,
			Skip:            false,
			SplitBySelector: true,
		},
		ResultFormat: resultFormat(c),
	}, nil
}

func resultFormat(c runnerplugin.Config) string {
	if c.ResultPath == "" {
		return ""
	}
	if strings.HasSuffix(c.ResultPath, ".xml") {
		return "junit"
	}
	return "json"
}

// DiscoverTargets returns the discovery pattern, for bktest to find file
// names with. Custom selector splitting without a selector list falls back to
// file discovery.
func (handler) DiscoverTargets(request runnerplugin.DiscoverTargetsRequest) (runnerplugin.DiscoverTargetsResponse, error) {
	c := request.Config
	fmt.Fprintln(os.Stderr, "Buildkite Test Engine Client: --selector-file (or BUILDKITE_TEST_ENGINE_SELECTOR_FILE) is not set for the custom runner. Discovering selector values with the configured test file pattern.")
	if c.TestFilePattern == "" {
		// bktest only finds files with a pattern, and an empty pattern
		// matches no files.
		return runnerplugin.DiscoverTargetsResponse{}, fmt.Errorf("no files found with pattern %q and exclude pattern %q", c.TestFilePattern, c.TestFileExcludePattern)
	}
	return runnerplugin.DiscoverTargetsResponse{Pattern: c.TestFilePattern, ExcludePattern: c.TestFileExcludePattern}, nil
}

func (handler) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error) {
	var response runnerplugin.InterpretAttemptResponse
	resultPath := request.Config.ResultPath

	// If the result path is not set, bubble up the error directly.
	if resultPath == "" {
		return response, nil
	}

	if strings.HasSuffix(resultPath, ".xml") {
		tests, parseErr := runnerplugin.ReadJUnitXML(resultPath)
		if parseErr != nil {
			response.Messages = append(response.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read JUnit XML output: %v", parseErr))
			return response, nil
		}
		for _, test := range tests {
			response.Tests = append(response.Tests, runnerplugin.TestResult{
				TestCase: runnerplugin.TestCase{
					Format: runnerplugin.FormatExample,
					Scope:  test.Classname,
					Name:   test.Name,
					Path:   test.Classname,
				},
				Status: test.Result,
			})
		}
	} else {
		tests, parseErr := runnerplugin.ReadTestEngineResults(resultPath)
		if parseErr != nil {
			response.Messages = append(response.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read test engine JSON output: %v", parseErr))
			return response, nil
		}
		for _, test := range tests {
			response.Tests = append(response.Tests, runnerplugin.TestResult{
				TestCase: runnerplugin.TestCase{
					Identifier: test.ID,
					Format:     runnerplugin.FormatExample,
					Scope:      test.Scope,
					Name:       test.Name,
					Path:       fmt.Sprintf("%s:%s", test.FileName, test.Location),
				},
				Status: test.Result,
			})
		}
	}

	return response, nil
}

// PrepareAttempt adds the test cases to the command before splitting it
// into words, so a test case with a space is more than one word.
func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	cmd := request.Config.TestCommand
	if request.Retry {
		cmd = request.Config.RetryTestCommand
	}

	testPaths := runnerplugin.Paths(request.TestCases)

	cmd = strings.Replace(cmd, "{{testExamples}}", strings.Join(testPaths, " "), 1)

	words, err := shellquote.Split(cmd)
	if err != nil {
		return runnerplugin.PrepareAttemptResponse{}, err
	}

	return runnerplugin.PrepareAttemptResponse{Argv: words}, nil
}
