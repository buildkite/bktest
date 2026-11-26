package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
	"github.com/kballard/go-shellquote"
	"golang.org/x/mod/semver"
)

const pytestCollectionErrorTag = "test.pytest_collection_error"

type pytestState struct {
	UseJUnit bool `json:"use_junit"`
}

type pytestPlugin struct{}

func main() {
	plugin.Serve(&pytestPlugin{})
}

func (p *pytestPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	pluginInstalled := cfg.PersistentRunner || checkPythonPackageInstalled("buildkite_test_collector")
	useJUnit := !pluginInstalled

	if pluginInstalled {
		if cfg.TagFilters != "" {
			if err := checkBuildkiteTestCollectorVersion("1.2.0"); err != nil {
				return plugin.ConfigureResponse{}, fmt.Errorf("%v\nPlease upgrade with: pip install --upgrade buildkite-test-collector", err)
			}
		}
		if cfg.TestCommand == "" {
			cfg.TestCommand = "pytest {{testExamples}} --json={{resultPath}}"
		}
		if cfg.ResultPath == "" {
			cfg.ResultPath = getRandomTempFilename("pytest-results.json")
		}
	} else {
		if cfg.TagFilters != "" {
			return plugin.ConfigureResponse{}, fmt.Errorf("--tag-filters requires the 'buildkite-test-collector' package. Please install it with: pip install buildkite-test-collector")
		}
		if cfg.TestCommand == "" {
			cfg.TestCommand = "pytest {{testExamples}} --junit-xml={{resultPath}}"
		}
		if cfg.ResultPath == "" {
			cfg.ResultPath = getRandomTempFilename("pytest-results.xml")
		}
	}

	if strings.Contains(cfg.TestCommand, "--junit-xml") {
		useJUnit = true
	} else if strings.Contains(cfg.TestCommand, "--json=") {
		if !pluginInstalled {
			return plugin.ConfigureResponse{}, fmt.Errorf("--json requires the 'buildkite-test-collector' package to be installed. Please install it with: pip install buildkite-test-collector")
		}
		useJUnit = false
	}

	var warning string
	var resultFormat string
	batchFormat := "pytest-json"
	if useJUnit {
		warning = "Buildkite Test Engine Client: Using JUnit XML output for pytest results."
		resultFormat = "junit"
		batchFormat = "pytest-junit"
	} else {
		warning = "Buildkite Test Engine Client: Using JSON output for pytest results."
		resultFormat = ""
	}

	if cfg.TestFilePattern == "" {
		cfg.TestFilePattern = "**/{*_test,test_*}.py"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}

	stateBytes, _ := json.Marshal(pytestState{UseJUnit: useJUnit})

	return plugin.ConfigureResponse{
		RunnerID:                  "pytest",
		Name:                      "pytest",
		Config:                    cfg,
		ResultFormat:              resultFormat,
		BatchReportFormat:         batchFormat,
		RequiresResultPath:        false,
		PoolNonRetryableExitCodes: true,
		Features: plugin.SupportedFeatures{
			SplitByFile:     true,
			SplitByExample:  true,
			SplitBySelector: true,
			FilterTestFiles: true,
			FilterTestByTag: true,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
		},
		Capabilities: plugin.Capabilities{
			DiscoverTargets:  true,
			DiscoverExamples: true,
			InterpretAttempt: true,
			ParseBatchReport: true,
		},
		Warnings: []string{warning},
		State:    stateBytes,
	}, nil
}

func (p *pytestPlugin) DiscoverTargets(req plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	files, err := plugin.DiscoverFiles(req.Config.TestFilePattern, req.Config.TestFileExcludePattern)
	if err != nil {
		return plugin.DiscoverTargetsResponse{}, err
	}
	return plugin.DiscoverTargetsResponse{Targets: files}, nil
}

func (p *pytestPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	cmd := req.Config.TestCommand
	if req.Retry {
		cmd = req.Config.RetryTestCommand
	}

	testPaths := plugin.PathsFromTestCases(req.TestCases)
	testExamples := shellquote.Join(testPaths...)

	if strings.Contains(cmd, "{{testExamples}}") {
		cmd = strings.Replace(cmd, "{{testExamples}}", testExamples, 1)
	} else {
		cmd = cmd + " " + testExamples
	}
	cmd = strings.Replace(cmd, "{{resultPath}}", req.Config.ResultPath, 1)

	args, err := plugin.SplitCommand(cmd)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}

	return plugin.PrepareAttemptResponse{
		Command: args[0],
		Args:    args[1:],
	}, nil
}

func (p *pytestPlugin) DiscoverExamples(req plugin.DiscoverExamplesRequest) (plugin.DiscoverExamplesResponse, error) {
	if len(req.Files) == 0 {
		return plugin.DiscoverExamplesResponse{Examples: []plugin.TestCase{}}, nil
	}

	state := decodeState(req.State)
	args := []string{"--collect-only", "-q"}
	if req.Config.TagFilters != "" {
		args = append(args, "--tag-filters", req.Config.TagFilters)
	}
	args = append(args, req.Files...)
	cmd := exec.Command("pytest", args...)

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return plugin.DiscoverExamplesResponse{}, fmt.Errorf("pytest collection failed: %s", exitErr.Stderr)
		}
		return plugin.DiscoverExamplesResponse{}, fmt.Errorf("pytest collection failed: %w", err)
	}

	var examples []plugin.TestCase
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "::") {
			continue
		}
		examples = append(examples, mapNodeIDToTestCase(line, state.UseJUnit))
	}
	return plugin.DiscoverExamplesResponse{Examples: examples}, nil
}

func (p *pytestPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	state := decodeState(req.State)
	parseExit2JSON := false

	if req.CommandFailed && (req.Signaled || req.ExitCode != 1) {
		if state.UseJUnit || req.Signaled || req.ExitCode != 2 {
			return plugin.InterpretAttemptResponse{}, nil
		}
		parseExit2JSON = true
	}

	var resp plugin.InterpretAttemptResponse
	var parseErr error
	if state.UseJUnit {
		resp, parseErr = interpretJUnit(req.Config.ResultPath)
	} else {
		resp, parseErr = interpretJSON(req.Config.ResultPath)
	}
	if parseErr != nil {
		resp.ParseWarning = fmt.Sprintf("Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: %v", parseErr)
	}
	if parseExit2JSON && resp.RunError == "" {
		resp.UseCommandErrorAsRunError = true
	}
	return resp, nil
}

func (p *pytestPlugin) ParseBatchReport(req plugin.ParseBatchReportRequest) (plugin.ParseBatchReportResponse, error) {
	data, err := base64.StdEncoding.DecodeString(req.ReportBase64)
	if err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("decode report: %w", err)
	}
	switch req.Format {
	case "pytest-json":
		return parsePytestJSONBatchReport(data)
	case "pytest-junit":
		return parsePytestJUnitBatchReport(data)
	default:
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("unsupported runner report format %q", req.Format)
	}
}

func decodeState(raw json.RawMessage) pytestState {
	var state pytestState
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &state)
	}
	return state
}

func interpretJSON(path string) (plugin.InterpretAttemptResponse, error) {
	tests, err := plugin.ParseTestEngineTestResult(path)
	if err != nil {
		return plugin.InterpretAttemptResponse{}, err
	}
	resp := plugin.InterpretAttemptResponse{}
	for _, test := range tests {
		p := pytestPathFromTestEngineResult(test.Scope, test.Name)
		resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
			TestCase: plugin.TestCase{
				Identifier: test.ID,
				Format:     plugin.TestCaseFormatExample,
				Scope:      test.Scope,
				Name:       test.Name,
				Path:       p,
			},
			Status: test.Result,
		})
		if test.Tags[pytestCollectionErrorTag] == "true" {
			resp.RunError = fmt.Sprintf("pytest collection failed: %s", p)
		}
	}
	return resp, nil
}

func interpretJUnit(path string) (plugin.InterpretAttemptResponse, error) {
	tests, err := plugin.LoadAndParseJUnitXML(path)
	if err != nil {
		return plugin.InterpretAttemptResponse{}, err
	}
	resp := plugin.InterpretAttemptResponse{}
	for _, test := range tests {
		p := pytestNodeIDFromJUnit(test.Classname, test.Name)
		resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
			TestCase: plugin.TestCase{
				Identifier: p,
				Format:     plugin.TestCaseFormatExample,
				Scope:      test.Classname,
				Name:       test.Name,
				Path:       p,
			},
			Status: test.Result,
		})
	}
	return resp, nil
}

func parsePytestJSONBatchReport(data []byte) (plugin.ParseBatchReportResponse, error) {
	var tests *[]plugin.TestEngineTest
	if err := json.Unmarshal(data, &tests); err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("invalid pytest JSON report: %w", err)
	}
	if tests == nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("invalid pytest JSON report: not a list of tests")
	}
	var resp plugin.ParseBatchReportResponse
	seen := map[string]int{}
	for _, test := range *tests {
		if test.Tags[pytestCollectionErrorTag] == "true" {
			file, _, _ := strings.Cut(pytestPathFromTestEngineResult(test.Scope, test.Name), "::")
			if !slices.Contains(resp.ErroredFiles, file) {
				resp.ErroredFiles = append(resp.ErroredFiles, file)
			}
			continue
		}
		p := pytestPathFromTestEngineResult(test.Scope, test.Name)
		if err := addPytestBatchTest(&resp, seen, plugin.TestCase{Scope: test.Scope, Name: test.Name, Path: p}, test.Result); err != nil {
			return plugin.ParseBatchReportResponse{}, err
		}
	}
	return resp, nil
}

func parsePytestJUnitBatchReport(data []byte) (plugin.ParseBatchReportResponse, error) {
	tests, err := plugin.ParseJUnitXML(data)
	if err != nil {
		return plugin.ParseBatchReportResponse{}, fmt.Errorf("invalid pytest JUnit XML report: %w", err)
	}
	var resp plugin.ParseBatchReportResponse
	seen := map[string]int{}
	for _, test := range tests {
		if test.Classname == "" {
			if test.Result != plugin.TestStatusSkipped {
				resp.ErrorsOutsideTests = true
			}
			continue
		}
		p := pytestNodeIDFromJUnit(test.Classname, test.Name)
		if err := addPytestBatchTest(&resp, seen, plugin.TestCase{Scope: test.Classname, Name: test.Name, Path: p}, test.Result); err != nil {
			return plugin.ParseBatchReportResponse{}, err
		}
	}
	return resp, nil
}

func addPytestBatchTest(resp *plugin.ParseBatchReportResponse, seen map[string]int, tc plugin.TestCase, status string) error {
	switch status {
	case plugin.TestStatusPassed, plugin.TestStatusFailed, plugin.TestStatusSkipped:
	default:
		return fmt.Errorf("unknown pytest test status %q", status)
	}
	file, _, _ := strings.Cut(tc.Path, "::")
	if file == "" || tc.Name == "" {
		return fmt.Errorf("pytest test %q has no file or name", tc.Path)
	}
	if i, ok := seen[tc.Path]; ok {
		if status == plugin.TestStatusFailed || resp.Tests[i].Status == plugin.TestStatusSkipped {
			resp.Tests[i].Status = status
		}
		return nil
	}
	tc.Identifier = tc.Path
	seen[tc.Path] = len(resp.Tests)
	resp.Tests = append(resp.Tests, plugin.BatchReportedTest{
		TestCase: tc,
		Status:   status,
		Selector: file,
	})
	return nil
}

func pytestPathFromTestEngineResult(scope, name string) string {
	if scope == "" {
		return name
	}
	return fmt.Sprintf("%s::%s", scope, name)
}

func pytestNodeIDFromJUnit(classname, name string) string {
	if classname == "" {
		return name
	}
	parts := strings.Split(classname, ".")

	moduleEnd := -1
	for i, part := range parts {
		if strings.HasPrefix(part, "test_") || strings.HasSuffix(part, "_test") {
			moduleEnd = i + 1
		}
	}
	if moduleEnd == -1 {
		moduleEnd = len(parts)
		for i, part := range parts {
			if len(part) > 0 && part[0] >= 'A' && part[0] <= 'Z' {
				moduleEnd = i
				break
			}
		}
	}

	modulePath := strings.Join(parts[:moduleEnd], "/") + ".py"
	classParts := parts[moduleEnd:]
	if len(classParts) == 0 {
		return modulePath + "::" + name
	}
	return modulePath + "::" + strings.Join(classParts, "::") + "::" + name
}

func mapNodeIDToTestCase(nodeID string, useJUnit bool) plugin.TestCase {
	lastIdx := strings.LastIndex(nodeID, "::")
	scope := ""
	name := nodeID
	if lastIdx != -1 {
		scope = nodeID[:lastIdx]
		name = nodeID[lastIdx+2:]
	}
	if useJUnit {
		scope = collectorScopeToJunitScope(scope)
	}
	return plugin.TestCase{
		Identifier: nodeID,
		Path:       nodeID,
		Scope:      scope,
		Name:       name,
		Format:     plugin.TestCaseFormatExample,
	}
}

func collectorScopeToJunitScope(scope string) string {
	path, rest, hasClass := strings.Cut(scope, "::")
	path = strings.TrimSuffix(path, ".py")
	if hasClass {
		path = path + "::" + rest
	}
	classname := strings.ReplaceAll(path, "::", ".")
	classname = strings.ReplaceAll(classname, "/", ".")
	return classname
}

func getRandomTempFilename(filename string) string {
	tempDir, err := os.MkdirTemp("", "bktec-pytest-*")
	if err != nil {
		panic(err)
	}
	return filepath.Join(tempDir, filename)
}

func getPythonPackageVersion(pkgName string) (string, error) {
	pythonCmd := exec.Command("python", "-c", "import importlib.metadata, sys; print(importlib.metadata.version(sys.argv[1]))", pkgName)
	output, err := pythonCmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not determine %s version: %w", pkgName, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func checkBuildkiteTestCollectorVersion(requiredVersion string) error {
	installedVersionStr, err := getPythonPackageVersion("buildkite-test-collector")
	if err != nil {
		return err
	}
	installedCanonical := "v" + installedVersionStr
	requiredCanonical := "v" + requiredVersion
	if !semver.IsValid(installedCanonical) {
		return fmt.Errorf("could not parse installed buildkite-test-collector version %q", installedVersionStr)
	}
	if !semver.IsValid(requiredCanonical) {
		return fmt.Errorf("could not parse required version %q", requiredVersion)
	}
	if semver.Compare(installedCanonical, requiredCanonical) <= 0 {
		return fmt.Errorf("buildkite-test-collector version %s is installed, but version >%s is required for --tag-filters support", installedVersionStr, requiredVersion)
	}
	return nil
}

func checkPythonPackageInstalled(pkgName string) bool {
	pythonCmd := exec.Command("python", "-c", "import importlib.util, sys; print(importlib.util.find_spec(sys.argv[1]) is not None)", pkgName)
	output, err := pythonCmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "True"
}
