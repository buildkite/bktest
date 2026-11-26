// Package cucumber is the runner plugin for Cucumber (Ruby implementation).
// It follows very similar behaviour to the RSpec runner. We rely on the JSON formatter
// so users MUST include `--format json --out {{resultPath}}` in their custom commands.
//
// We treat every Scenario as an individual test case. A scenario is considered failed
// if any step in it failed or has undefined status. "pending" and "skipped" are
// mapped to skipped.
package cucumber

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

type handler struct{}

// New returns the Cucumber handler.
func New() runnerplugin.Handler { return handler{} }

var (
	_ runnerplugin.ExampleDiscoverer  = handler{}
	_ runnerplugin.AttemptInterpreter = handler{}
)

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	if c.TestCommand == "" {
		// The pretty formatter gives a nice progress bar in the console, the JSON formatter is required for bktec.
		c.TestCommand = "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}"
	}

	if c.TestFilePattern == "" {
		c.TestFilePattern = "features/**/*.feature"
	}

	if c.RetryTestCommand == "" {
		c.RetryTestCommand = c.TestCommand
	}

	return runnerplugin.ConfigureResponse{
		Name:   "Cucumber",
		Config: c,
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			FilterTestByTag: false,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
			SplitBySelector: true,
		},
		ResultFormat: "cucumber-json",
	}, nil
}

// InterpretAttempt records the results of the Cucumber command.
func (handler) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error) {
	var response runnerplugin.InterpretAttemptResponse

	// Cucumber exits with a non-zero status code when there are test failures,
	// so we should always attempt to parse the report even if the command returns an error.
	report, parseErr := ParseReport(request.Config.ResultPath)
	if parseErr != nil {
		// We don't want to fail the build if we fail to parse the report,
		// therefore the attempt's error stays the command error (which can be nil), instead of the parse error.
		response.Messages = append(response.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read Cucumber JSON output, tests will not be retried: %v", parseErr))
		return response, nil
	}

	// Iterate scenarios.
	for _, feature := range report {
		for _, scenario := range feature.Elements {
			if scenario.Type != "scenario" {
				continue
			}
			status := scenario.AggregatedStatus()
			var testStatus string
			switch status {
			case "failed", "undefined", "errored":
				testStatus = runnerplugin.StatusFailed
			case "passed":
				testStatus = runnerplugin.StatusPassed
			case "pending", "skipped" /* cucumber-js uses skipped */ :
				testStatus = runnerplugin.StatusSkipped
			default:
				testStatus = runnerplugin.StatusUnknown
			}

			fileLinePath := fmt.Sprintf("%s:%d", feature.URI, scenario.Line)
			testCaseForResult := runnerplugin.TestCase{
				Identifier: fileLinePath, // Use file:line as the primary identifier
				Name:       scenario.Name,
				Scope:      feature.Name,
				Path:       fileLinePath,
			}

			response.Tests = append(response.Tests, runnerplugin.TestResult{TestCase: testCaseForResult, Status: testStatus})
		}
	}

	return response, nil
}

// mapScenarioToTestCase maps a Cucumber scenario (element) to a test case
func mapScenarioToTestCase(featureName string, featureURI string, scenario Element) runnerplugin.TestCase {
	// Cucumber scenarios are identified by file_path:line_number
	identifier := fmt.Sprintf("%s:%d", featureURI, scenario.Line)
	return runnerplugin.TestCase{
		Path:       identifier,
		Scope:      featureName,
		Name:       scenario.Name,
		Identifier: identifier, // Or scenario.ID if it's more suitable and consistently available
	}
}

// DiscoverExamples returns an array of test scenarios within the given feature files.
func (handler) DiscoverExamples(request runnerplugin.DiscoverExamplesRequest) (runnerplugin.DiscoverExamplesResponse, error) {
	response := runnerplugin.DiscoverExamplesResponse{TestCases: []runnerplugin.TestCase{}}
	files := request.Files
	debugf := request.Config.Debugf
	if len(files) == 0 {
		return response, nil
	}

	// Create a temporary file to store the JSON output of the cucumber dry run.
	f, err := os.CreateTemp("", "cucumber-dry-run-*.json")
	if err != nil {
		return response, fmt.Errorf("failed to create temporary file for cucumber dry run: %w", err)
	}
	debugf("Created temp file for cucumber dry run: %s", f.Name())

	defer func() {
		closeErr := f.Close()
		if closeErr != nil {
			debugf("Error closing temp file %s: %v", f.Name(), closeErr)
		}
		removeErr := os.Remove(f.Name())
		if removeErr != nil {
			debugf("Error removing temp file %s: %v", f.Name(), removeErr)
		}
	}()

	// Only the command's name is used: the dry run has its own arguments.
	words, err := commandWords(request.Config, runnerplugin.TestCasesFromPaths(files), false)
	if err != nil {
		return response, err
	}
	cmdName := words[0]

	dryRunArgs := append(
		[]string{"--dry-run", "--format", "json", "--out", f.Name(), "--format", "progress"},
		files...,
	)

	debugf("Running `%s %s` for dry run", cmdName, strings.Join(dryRunArgs, " "))

	// #nosec G204 -- the test command is configured by the bktest user.
	output, err := exec.Command(cmdName, dryRunArgs...).CombinedOutput()
	if err != nil {
		return response, fmt.Errorf("failed to run Cucumber dry run: %s", output)
	}

	dryRunReport, parseErr := parseDryRunJSONOutput(f.Name()) // Use parser from result_parser.go
	if parseErr != nil {
		return response, fmt.Errorf("failed to parse cucumber dry run JSON report from %s: %w", f.Name(), parseErr)
	}

	for _, feature := range dryRunReport {
		for _, scenario := range feature.Elements {
			if scenario.Type == "scenario" { // Only include scenarios, not scenario outlines directly (examples are handled differently)
				response.TestCases = append(response.TestCases, mapScenarioToTestCase(feature.Name, feature.URI, scenario))
			} else if scenario.Type == "scenario_outline" && scenario.Keyword == "Scenario Outline" {
				// Scenario outlines themselves aren't runnable directly by path:line of the outline.
				// Cucumber expands them into concrete scenarios based on their Examples tables.
				// The JSON from a dry run might already include these expanded examples as individual 'scenario' type elements.
				// If not, we'd need to parse scenario.Examples and generate test cases for each example row.
				// For now, we assume the JSON includes expanded examples as type: "scenario".
				// If the dry run JSON for outlines is different, this part needs adjustment.
				// Let's log if we encounter an outline to see its structure.
				debugf("Encountered Scenario Outline: %s:%d. Its examples might be listed as separate scenarios.", feature.URI, scenario.Line)
			}
		}
	}

	return response, nil
}

// PrepareAttempt replaces placeholders and returns command + args.
func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	words, err := commandWords(request.Config, request.TestCases, request.Retry)
	return runnerplugin.PrepareAttemptResponse{Argv: words}, err
}

func commandWords(c runnerplugin.Config, testCases []runnerplugin.TestCase, retry bool) ([]string, error) {
	testPaths := runnerplugin.Paths(testCases)

	cmd := c.TestCommand
	if retry {
		cmd = c.RetryTestCommand
	}

	words, err := shellquote.Split(cmd)
	if err != nil {
		return nil, err
	}

	idx := slices.Index(words, "{{testExamples}}")
	if idx < 0 {
		words = append(words, testPaths...)
	} else {
		words = slices.Replace(words, idx, idx+1, testPaths...)
	}

	idx = slices.Index(words, "{{resultPath}}")
	if idx >= 0 {
		words = slices.Replace(words, idx, idx+1, c.ResultPath)
	}

	return words, nil
}

// ---------------- Report parsing -------------------
// ParseReport uses Feature from result_parser.go

// ParseReport reads the Cucumber JSON report at path.
func ParseReport(path string) ([]Feature, error) {
	var report []Feature
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cucumber output: %v", err)
	}

	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("failed to parse cucumber output: %s", err)
	}

	return report, nil
}
