package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/plugin"
)

const (
	goTestResultFormatJUnit   = "junit"
	goTestResultFormatGoJSONL = "go-jsonl"
)

type goTestState struct {
	ResultFormat string `json:"result_format"`
}

type gotestPlugin struct{}

func main() {
	plugin.Serve(&gotestPlugin{})
}

func (p *gotestPlugin) Configure(req plugin.ConfigureRequest) (plugin.ConfigureResponse, error) {
	cfg := req.Config
	if cfg.TestCommand == "" {
		cfg.TestCommand = "gotestsum --jsonfile={{resultPath}} {{packages}}"
	}
	if cfg.RetryTestCommand == "" {
		cfg.RetryTestCommand = cfg.TestCommand
	}

	resultFormat := goTestResultFormatJUnit
	if commandProducesGoJSONL(cfg.TestCommand) {
		resultFormat = goTestResultFormatGoJSONL
	}
	stateBytes, _ := json.Marshal(goTestState{ResultFormat: resultFormat})

	return plugin.ConfigureResponse{
		RunnerID:           "gotest",
		Name:               "gotest",
		Config:             cfg,
		ResultFormat:       resultFormat,
		RequiresResultPath: true,
		Features: plugin.SupportedFeatures{
			SplitByFile:     false,
			SplitByExample:  false,
			SplitBySelector: true,
			FilterTestFiles: false,
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

func (p *gotestPlugin) DiscoverTargets(_ plugin.DiscoverTargetsRequest) (plugin.DiscoverTargetsResponse, error) {
	cmd := exec.Command("go", "list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", "./...")
	output, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return plugin.DiscoverTargetsResponse{}, fmt.Errorf("go list failed: %w\nstderr:\n%s", err, string(ee.Stderr))
		}
		return plugin.DiscoverTargetsResponse{}, fmt.Errorf("failed to run go list: %w", err)
	}
	packages := strings.Split(strings.TrimSpace(string(output)), "\n")
	validPackages := []string{}
	for _, pkg := range packages {
		if pkg != "" {
			validPackages = append(validPackages, pkg)
		}
	}
	if len(validPackages) == 0 {
		return plugin.DiscoverTargetsResponse{}, fmt.Errorf("no Go packages with tests found using `go list`")
	}
	return plugin.DiscoverTargetsResponse{Targets: validPackages}, nil
}

func (p *gotestPlugin) PrepareAttempt(req plugin.PrepareAttemptRequest) (plugin.PrepareAttemptResponse, error) {
	packages, err := getPackages(req.TestCases)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, fmt.Errorf("failed to generate test package list: %w", err)
	}

	cmdStr := req.Config.TestCommand
	if req.Retry {
		cmdStr = req.Config.RetryTestCommand
	}

	concatenatedPackages := strings.Join(packages, " ")
	if strings.Contains(cmdStr, "{{packages}}") {
		cmdStr = strings.Replace(cmdStr, "{{packages}}", concatenatedPackages, 1)
	} else {
		cmdStr = cmdStr + " " + concatenatedPackages
	}
	cmdStr = strings.Replace(cmdStr, "{{resultPath}}", req.Config.ResultPath, 1)

	args, err := plugin.SplitCommand(cmdStr)
	if err != nil {
		return plugin.PrepareAttemptResponse{}, err
	}

	state := decodeState(req.State, req.Config.TestCommand)
	resp := plugin.PrepareAttemptResponse{
		Command: args[0],
		Args:    args[1:],
	}
	if state.ResultFormat == goTestResultFormatGoJSONL && isGoTestJSONCommand(args) && goJSONLFileArg(args) == "" {
		resp.StdoutTeePath = req.Config.ResultPath
	}
	return resp, nil
}

func (p *gotestPlugin) InterpretAttempt(req plugin.InterpretAttemptRequest) (plugin.InterpretAttemptResponse, error) {
	if req.CommandFailed && (req.Signaled || req.ExitCode != 1) {
		return plugin.InterpretAttemptResponse{}, nil
	}

	state := decodeState(req.State, req.Config.TestCommand)
	var resp plugin.InterpretAttemptResponse
	var parseErr error
	switch state.ResultFormat {
	case goTestResultFormatJUnit:
		resp, parseErr = parseJUnitResults(req.Config.ResultPath)
	case goTestResultFormatGoJSONL:
		resp, parseErr = parseGoJSONLResults(req.Config.ResultPath)
	default:
		parseErr = fmt.Errorf("unsupported Go test result format %q", state.ResultFormat)
	}
	if parseErr != nil {
		return plugin.InterpretAttemptResponse{
			ParseWarning: fmt.Sprintf("Buildkite Test Engine Client: Failed to read Go test output, tests will not be retried: %v", parseErr),
		}, nil
	}
	return resp, nil
}

func decodeState(raw json.RawMessage, testCmd string) goTestState {
	var state goTestState
	if len(raw) > 0 && json.Unmarshal(raw, &state) == nil && state.ResultFormat != "" {
		return state
	}
	if commandProducesGoJSONL(testCmd) {
		return goTestState{ResultFormat: goTestResultFormatGoJSONL}
	}
	return goTestState{ResultFormat: goTestResultFormatJUnit}
}

func parseJUnitResults(path string) (plugin.InterpretAttemptResponse, error) {
	testResults, err := plugin.LoadAndParseJUnitXML(path)
	if err != nil {
		return plugin.InterpretAttemptResponse{}, err
	}
	resp := plugin.InterpretAttemptResponse{}
	for _, test := range testResults {
		if isBuildFailure(test) {
			resp.RunError = fmt.Sprintf("go test failed to build %s", test.SuiteName)
			continue
		}
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

type goJSONLTestEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	FailedBuild string `json:"FailedBuild"`
}

func parseGoJSONLResults(path string) (plugin.InterpretAttemptResponse, error) {
	events, err := loadGoJSONLTestEvents(path)
	if err != nil {
		return plugin.InterpretAttemptResponse{}, err
	}

	packageOutputs := map[string]string{}
	packageFailed := map[string]bool{}
	packageBuildFailed := map[string]bool{}
	packageHasFailedTest := map[string]bool{}
	testStatuses := map[string]string{}
	testCases := map[string]plugin.TestCase{}
	testOrder := []string{}
	packageOrder := []string{}

	for _, event := range events {
		if event.Package == "" {
			continue
		}
		if event.Output != "" {
			packageOutputs[event.Package] += event.Output
		}
		if event.Test == "" {
			if event.Action == "fail" {
				if !packageFailed[event.Package] {
					packageOrder = append(packageOrder, event.Package)
				}
				packageFailed[event.Package] = true
				if event.FailedBuild != "" {
					packageBuildFailed[event.Package] = true
				}
			}
			continue
		}

		status, ok := goJSONLActionStatus(event.Action)
		if !ok {
			continue
		}

		key := event.Package + "/" + event.Test
		if _, seen := testCases[key]; !seen {
			testOrder = append(testOrder, key)
		}
		if testStatuses[key] != plugin.TestStatusFailed {
			testStatuses[key] = status
		}
		if status == plugin.TestStatusFailed {
			packageHasFailedTest[event.Package] = true
		}
		testCases[key] = plugin.TestCase{
			Format: plugin.TestCaseFormatExample,
			Scope:  event.Package,
			Name:   event.Test,
			Path:   event.Package,
		}
	}

	resp := plugin.InterpretAttemptResponse{}
	for _, key := range testOrder {
		resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
			TestCase: testCases[key],
			Status:   testStatuses[key],
		})
	}

	for _, pkg := range packageOrder {
		if packageBuildFailed[pkg] || strings.Contains(packageOutputs[pkg], "[build failed]") {
			resp.RunError = fmt.Sprintf("go test failed to build %s", pkg)
			continue
		}
		if !packageHasFailedTest[pkg] {
			resp.Results = append(resp.Results, plugin.AttemptTestOutcome{
				TestCase: plugin.TestCase{
					Format: plugin.TestCaseFormatExample,
					Scope:  pkg,
					Name:   "TestMain",
					Path:   pkg,
				},
				Status: plugin.TestStatusFailed,
			})
		}
	}

	return resp, nil
}

func loadGoJSONLTestEvents(path string) ([]goJSONLTestEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read go jsonl: %w", err)
	}
	defer file.Close()

	var events []goJSONLTestEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event goJSONLTestEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("failed to parse go jsonl: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read go jsonl: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("failed to parse go jsonl: no events found")
	}
	return events, nil
}

func goJSONLActionStatus(action string) (string, bool) {
	switch action {
	case "pass":
		return plugin.TestStatusPassed, true
	case "fail":
		return plugin.TestStatusFailed, true
	case "skip":
		return plugin.TestStatusSkipped, true
	default:
		return plugin.TestStatusUnknown, false
	}
}

func isBuildFailure(test plugin.JUnitXMLTestCase) bool {
	return test.Name == "TestMain" &&
		test.Classname == "" &&
		test.Failure != nil &&
		strings.Contains(test.Failure.Content, "[build failed]")
}

func commandProducesGoJSONL(command string) bool {
	args, err := plugin.SplitCommand(command)
	if err != nil {
		return strings.Contains(command, "--jsonfile") || strings.Contains(command, "-json")
	}
	return isGoTestJSONCommand(args) || goJSONLFileArg(args) != ""
}

func hasGoTestJSONArg(args []string) bool {
	for _, arg := range args {
		if arg == "-json" {
			return true
		}
	}
	return false
}

func goJSONLFileArg(args []string) string {
	for i, arg := range args {
		if arg == "--jsonfile" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--jsonfile=") {
			return strings.TrimPrefix(arg, "--jsonfile=")
		}
	}
	return ""
}

func isGoTestJSONCommand(args []string) bool {
	return len(args) >= 3 && filepath.Base(args[0]) == "go" && args[1] == "test" && hasGoTestJSONArg(args)
}

func getPackages(testCases []plugin.TestCase) ([]string, error) {
	packages := make([]string, 0, len(testCases))
	seen := map[string]bool{}
	for _, tc := range testCases {
		pkg := packageFromTestCase(tc)
		if pkg == "" {
			continue
		}
		if !seen[pkg] {
			packages = append(packages, pkg)
			seen[pkg] = true
		}
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("unable to extract package names from test plan")
	}
	return packages, nil
}

func packageFromTestCase(tc plugin.TestCase) string {
	if tc.Format == plugin.TestCaseFormatSelector && tc.Value != "" {
		return tc.Value
	}
	if tc.Value != "" && tc.Path == "" {
		return tc.Value
	}
	return tc.Path
}
