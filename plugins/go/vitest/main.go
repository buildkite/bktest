package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

type vitestState struct {
	RetryNames map[string][]string `json:"retry_names,omitempty"`
}

type vitestPlugin struct{}

func main() {
	plugin.Serve(&vitestPlugin{})
}

func (p *vitestPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "npx vitest run {{testExamples}} --reporter=default --reporter=json --outputFile {{resultPath}}"
	}
	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = "npx vitest run --testNamePattern '{{testNamePattern}}' --reporter=default --reporter=json --outputFile {{resultPath}}"
	}
	stateBytes, _ := json.Marshal(vitestState{RetryNames: map[string][]string{}})

	return plugin.ConfigureResponse{
		RunnerID:           "vitest",
		Name:               "Vitest",
		Config:             cfg,
		ResultFormat:       "jest-json",
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
			ParseBatchReport: false,
		},
		State: stateBytes,
	}, nil
}

func (p *vitestPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *vitestPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
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

	state := decodeState(req.State)

	if req.Retry {
		idx := slices.Index(words, "{{testNamePattern}}")
		if idx < 0 {
			return plugin.PrepareAttemptResponse{}, fmt.Errorf("couldn't find '{{testNamePattern}}' sentinel in retry command")
		}

		var escapedTestCases []string
		for _, tc := range req.TestCases {
			names := state.RetryNames[plugin.TestIdentifier(tc)]
			if len(names) == 0 {
				fullName := tc.Name
				if tc.Scope != "" {
					fullName = tc.Scope + " " + tc.Name
				}
				names = []string{fullName}
			}
			for _, name := range names {
				escapedTestCases = append(escapedTestCases, regexp.QuoteMeta(name))
			}
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

func (p *vitestPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	state := decodeState(req.State)
	resp := plugin.InterpretAttemptResponse{
		FailIfFailedTests:        true,
		FailIfFailedTestsMessage: "Vitest finished with failed tests; see the report above",
	}

	report, parseErr := parseReport(req.Config.ResultPath)
	if parseErr != nil {
		resp.ParseWarning = fmt.Sprintf("Buildkite Test Engine Client: Failed to read Vitest output, tests will not be retried: %v", parseErr)
		resp.State = encodeState(state)
		return resp, nil
	}

	workDir, err := os.Getwd()
	if err != nil {
		return plugin.InterpretAttemptResponse{}, fmt.Errorf("failed to get current working directory: %v", err)
	}

	for _, testResult := range report.TestResults {
		if testResult.Status == "failed" && len(testResult.AssertionResults) == 0 {
			resp.RunError = "Vitest failed with runtime error test suites"
		}

		for _, example := range testResult.AssertionResults {
			var status string
			switch example.Status {
			case "failed":
				status = plugin.TestStatusFailed
			case "passed":
				status = plugin.TestStatusPassed
			case "pending", "skipped", "todo":
				status = plugin.TestStatusSkipped
			default:
				status = plugin.TestStatusUnknown
			}

			testPath, err := filepath.Rel(workDir, testResult.FileName)
			if err != nil {
				return plugin.InterpretAttemptResponse{}, fmt.Errorf("failed to get relative path of test file: %v", err)
			}

			tc := plugin.TestCase{
				Name:  example.Title,
				Scope: strings.Join(example.AncestorTitles, " "),
				Path:  testPath,
			}

			if req.Retry && status == plugin.TestStatusSkipped {
				continue
			}

			titles := append(slices.Clone(example.AncestorTitles), example.Title)
			state.RetryNames[plugin.TestIdentifier(tc)] = slices.Compact([]string{
				strings.Join(titles, " "),
				strings.Join(titles, " > "),
			})
			resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
				TestCase: tc,
				Status:   status,
			})
		}
	}

	resp.State = encodeState(state)
	return resp, nil
}

func decodeState(raw json.RawMessage) vitestState {
	state := vitestState{RetryNames: map[string][]string{}}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &state)
		if state.RetryNames == nil {
			state.RetryNames = map[string][]string{}
		}
	}
	return state
}

func encodeState(state vitestState) json.RawMessage {
	data, _ := json.Marshal(state)
	return data
}

type vitestReport struct {
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

func parseReport(path string) (vitestReport, error) {
	var report vitestReport
	data, err := os.ReadFile(path)
	if err != nil {
		return vitestReport{}, fmt.Errorf("failed to read report output: %v", err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return vitestReport{}, fmt.Errorf("failed to parse report output: %s", err)
	}
	return report, nil
}
