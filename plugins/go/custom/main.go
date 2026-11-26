package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

type customPlugin struct{}

func main() {
	plugin.Serve(&customPlugin{})
}

func (p *customPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		return plugin.ConfigureResponse{}, errors.New("test command must be provided for custom runner")
	}
	if cfg.TestFilePattern == "" && cfg.SelectorListPath == "" {
		return plugin.ConfigureResponse{}, errors.New("test file pattern must be provided for custom runner")
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}

	resultFormat := ""
	if cfg.ResultPath != "" {
		if strings.HasSuffix(cfg.ResultPath, ".xml") {
			resultFormat = "junit"
		} else {
			resultFormat = "json"
		}
	}

	return plugin.ConfigureResponse{
		RunnerID:     "custom",
		Name:         "Custom test runner",
		Config:       cfg,
		ResultFormat: resultFormat,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  false,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       false,
			Mute:            true,
			Skip:            false,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: false,
			InterpretAttempt: cfg.ResultPath != "",
			ParseBatchReport: false,
		},
	}, nil
}

func (p *customPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	fmt.Fprintln(os.Stderr, "Buildkite Test Engine Client: --selector-file (or BUILDKITE_TEST_ENGINE_SELECTOR_FILE) is not set for the custom runner. Discovering selector values with the configured test file pattern.")
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *customPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmd := req.Config.TestCommand
	if req.Retry {
		cmd = req.Config.RetryTestCommand
	}

	testPaths := plugin.PathsFromTestCases(req.TestCases)
	cmd = strings.Replace(cmd, "{{testExamples}}", strings.Join(testPaths, " "), 1)

	words, err := plugin.SplitCommand(cmd)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}

	return plugin.PrepareAttemptResponse{
		Command: words[0],
		Args:    words[1:],
	}, nil
}

func (p *customPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	if req.Config.ResultPath == "" {
		return plugin.InterpretAttemptResponse{}, nil
	}

	resp := plugin.InterpretAttemptResponse{}
	if strings.HasSuffix(req.Config.ResultPath, ".xml") {
		tests, parseErr := plugin.LoadAndParseJUnitXML(req.Config.ResultPath)
		if parseErr != nil {
			resp.ParseWarning = fmt.Sprintf("Buildkite Test Engine Client: Failed to read JUnit XML output: %v", parseErr)
			return resp, nil
		}
		for _, test := range tests {
			resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
				TestCase: plugin.TestCase{
					Format: plugin.TestCaseFormatExample,
					Scope:  test.Classname,
					Name:   test.Name,
					Path:   test.Classname,
				},
				Status: test.Result,
			})
		}
		return resp, nil
	}

	tests, parseErr := plugin.ParseTestEngineTestResult(req.Config.ResultPath)
	if parseErr != nil {
		resp.ParseWarning = fmt.Sprintf("Buildkite Test Engine Client: Failed to read test engine JSON output: %v", parseErr)
		return resp, nil
	}
	for _, test := range tests {
		resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
			TestCase: plugin.TestCase{
				Identifier: test.ID,
				Format:     plugin.TestCaseFormatExample,
				Scope:      test.Scope,
				Name:       test.Name,
				Path:       fmt.Sprintf("%s:%s", test.FileName, test.Location),
			},
			Status: test.Result,
		})
	}
	return resp, nil
}
