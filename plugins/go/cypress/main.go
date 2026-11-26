package main

import (
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

type cypressPlugin struct{}

func main() {
	plugin.Serve(&cypressPlugin{})
}

func (p *cypressPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "npx cypress run --spec {{testExamples}}"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}
	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "**/*.cy.{js,jsx,ts,tsx}"
	}
	return plugin.ConfigureResponse{
		RunnerID: "cypress",
		Name:     "Cypress",
		Config:   cfg,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  false,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       false,
			Mute:            false,
			Skip:            false,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: false,
			InterpretAttempt: false,
			ParseBatchReport: false,
		},
	}, nil
}

func (p *cypressPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *cypressPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmd := req.Config.TestCommand
	if req.Retry {
		cmd = req.Config.RetryTestCommand
	}
	words, err := plugin.SplitCommand(cmd)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}

	testPaths := plugin.PathsFromTestCases(req.TestCases)
	specs := strings.Join(testPaths, ",")

	idx := slices.Index(words, "{{testExamples}}")
	if idx < 0 {
		words = append(words, "--spec", specs)
	} else {
		words[idx] = specs
	}

	return plugin.PrepareAttemptResponse{
		Command: words[0],
		Args:    words[1:],
	}, nil
}
