package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

type cucumberPlugin struct{}

func main() {
	plugin.Serve(&cucumberPlugin{})
}

func (p *cucumberPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}"
	}
	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "features/**/*.feature"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}
	return plugin.ConfigureResponse{
		RunnerID:           "cucumber",
		Name:               "Cucumber",
		Config:             cfg,
		ResultFormat:       "cucumber-json",
		RequiresResultPath: true,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  true,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: true,
			InterpretAttempt: true,
			ParseBatchReport: false,
		},
	}, nil
}

func (p *cucumberPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *cucumberPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmdName, cmdArgs, err := commandNameAndArgs(req.Config, req.TestCases, req.Retry)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}
	return plugin.PrepareAttemptResponse{
		Command: cmdName,
		Args:    cmdArgs,
	}, nil
}

func (p *cucumberPlugin) DiscoverExamples(req plugin.DiscoverExamplesRequest) (plugin.DiscoverExamplesResponse, error) {
	if len(req.Files) == 0 {
		return plugin.DiscoverExamplesResponse{Examples: []plugin.TestCase{}}, nil
	}

	f, err := os.CreateTemp("", "cucumber-dry-run-*.json")
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to create temporary file for cucumber dry run: %w", err)
	}
	tmpPath := f.Name()
	f.Close()
	defer os.Remove(tmpPath)

	cmdName, _, err := commandNameAndArgs(req.Config, plugin.TestCasesFromPaths(req.Files), false)
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, err
	}

	dryRunArgs := append(
		[]string{"--dry-run", "--format", "json", "--out", tmpPath, "--format", "progress"},
		req.Files...,
	)

	output, err := exec.Command(cmdName, dryRunArgs...).CombinedOutput()
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to run Cucumber dry run: %s", output)
	}

	dryRunReport, err := parseCucumberJSONReport(tmpPath)
	if err != nil {
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("failed to parse cucumber dry run JSON report from %s: %w", tmpPath, err)
	}

	examples := []plugin.TestCase{}
	for _, feature := range dryRunReport {
		for _, scenario := range feature.Elements {
			if scenario.Type == "scenario" {
				identifier := fmt.Sprintf("%s:%d", feature.URI, scenario.Line)
				examples = append(examples, plugin.TestCase{
					Path:       identifier,
					Scope:      feature.Name,
					Name:       scenario.Name,
					Identifier: identifier,
				})
			}
		}
	}
	return plugin.DiscoverExamplesResponse{Examples: examples}, nil
}

func (p *cucumberPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	report, err := parseCucumberReport(req.Config.ResultPath)
	if err != nil {
		return plugin.InterpretAttemptResponse{
			ParseWarning: fmt.Sprintf("Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: %v", err),
		}, nil
	}

	resp := plugin.InterpretAttemptResponse{}
	for _, feature := range report {
		for _, scenario := range feature.Elements {
			if scenario.Type != "scenario" {
				continue
			}
			var status string
			switch scenario.aggregatedStatus() {
			case "failed", "undefined", "errored":
				status = plugin.TestStatusFailed
			case "passed":
				status = plugin.TestStatusPassed
			case "pending", "skipped":
				status = plugin.TestStatusSkipped
			default:
				status = plugin.TestStatusUnknown
			}
			fileLinePath := fmt.Sprintf("%s:%d", feature.URI, scenario.Line)
			resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
				TestCase: plugin.TestCase{
					Identifier: fileLinePath,
					Name:       scenario.Name,
					Scope:      feature.Name,
					Path:       fileLinePath,
				},
				Status: status,
			})
		}
	}
	return resp, nil
}

type cucumberFeature struct {
	URI      string            `json:"uri"`
	Name     string            `json:"name"`
	Elements []cucumberElement `json:"elements"`
}

type cucumberElement struct {
	Name    string         `json:"name"`
	Line    int            `json:"line"`
	Type    string         `json:"type"`
	Keyword string         `json:"keyword"`
	Steps   []cucumberStep `json:"steps"`
}

type cucumberStep struct {
	Result *struct {
		Status string `json:"status"`
	} `json:"result,omitempty"`
}

func (e cucumberElement) aggregatedStatus() string {
	status := "passed"
	for _, step := range e.Steps {
		if step.Result == nil {
			continue
		}
		switch step.Result.Status {
		case "failed", "undefined", "errored":
			return "failed"
		case "pending", "skipped":
			if status != "failed" {
				status = "pending"
			}
		}
	}
	return status
}

func parseCucumberReport(path string) ([]cucumberFeature, error) {
	var report []cucumberFeature
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cucumber output: %v", err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("failed to parse cucumber output: %s", err)
	}
	return report, nil
}

func parseCucumberJSONReport(filePath string) ([]cucumberFeature, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cucumber json report file %s: %w", filePath, err)
	}
	if len(data) == 0 {
		return []cucumberFeature{}, nil
	}
	var features []cucumberFeature
	if err := json.Unmarshal(data, &features); err != nil {
		var single cucumberFeature
		if singleErr := json.Unmarshal(data, &single); singleErr == nil {
			features = []cucumberFeature{single}
		} else {
			return nil, fmt.Errorf("failed to unmarshal cucumber json report from %s: %w. Single feature unmarshal error: %v", filePath, err, singleErr)
		}
	}
	return features, nil
}

func commandNameAndArgs(cfg plugin.RunnerSettings, testCases []plugin.TestCase, retry bool) (string, []string, error) {
	testPaths := plugin.PathsFromTestCases(testCases)

	cmd := cfg.TestCommand
	if retry {
		cmd = cfg.RetryTestCommand
	}

	words, err := plugin.SplitCommand(cmd)
	if err != nil {
		return "", []string{}, err
	}

	idx := slices.Index(words, "{{testExamples}}")
	if idx < 0 {
		words = append(words, testPaths...)
	} else {
		words = slices.Replace(words, idx, idx+1, testPaths...)
	}

	idx = slices.Index(words, "{{resultPath}}")
	if idx >= 0 {
		words = slices.Replace(words, idx, idx+1, cfg.ResultPath)
	}

	return words[0], words[1:], nil
}
