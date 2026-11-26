// Package pytest is the runner plugin for pytest.
package pytest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
	"golang.org/x/mod/semver"
)

type handler struct{}

// New returns the pytest handler.
func New() runnerplugin.Handler { return handler{} }

var (
	_ runnerplugin.ExampleDiscoverer  = handler{}
	_ runnerplugin.AttemptInterpreter = handler{}
	_ runnerplugin.BatchReportParser  = handler{}
)

// state is what Configure decided that later operations can't work out from
// the configuration alone.
type state struct {
	// UseJUnit is whether pytest's results are JUnit XML instead of
	// buildkite-test-collector's JSON.
	UseJUnit bool `json:"use_junit"`
}

const stateFile = "pytest.json"

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	pluginInstalled := collectorInstalled(c)

	useJUnit := !pluginInstalled

	if pluginInstalled {
		// Ensure buildkite-test-collector version is >1.2.0 for --tag-filters support
		if c.TagFilters != "" {
			if err := checkBuildkiteTestCollectorVersion("1.2.0"); err != nil {
				return runnerplugin.ConfigureResponse{}, fmt.Errorf("%w\nPlease upgrade with: pip install --upgrade buildkite-test-collector", err)
			}
		}
		if c.TestCommand == "" {
			c.TestCommand = "pytest {{testExamples}} --json={{resultPath}}"
		}
		if c.ResultPath == "" {
			path, err := getRandomTempFilename("pytest-results.json")
			if err != nil {
				return runnerplugin.ConfigureResponse{}, err
			}
			c.ResultPath = path
		}
	} else {
		if c.TagFilters != "" {
			//nolint:staticcheck // ST1005: the message is what bktest has always shown.
			return runnerplugin.ConfigureResponse{}, errors.New("--tag-filters requires the 'buildkite-test-collector' package.\nPlease install it with: pip install buildkite-test-collector.")
		}
		if c.TestCommand == "" {
			c.TestCommand = "pytest {{testExamples}} --junit-xml={{resultPath}}"
		}
		if c.ResultPath == "" {
			path, err := getRandomTempFilename("pytest-results.xml")
			if err != nil {
				return runnerplugin.ConfigureResponse{}, err
			}
			c.ResultPath = path
		}
	}

	// Detect output format from the result flag in the command, so a custom command
	// with --junit-xml works even when buildkite-test-collector is installed (and vice versa).
	if strings.Contains(c.TestCommand, "--junit-xml") {
		useJUnit = true
	} else if strings.Contains(c.TestCommand, "--json=") {
		if !pluginInstalled {
			//nolint:staticcheck // ST1005: the message is what bktest has always shown.
			return runnerplugin.ConfigureResponse{}, errors.New("--json requires the 'buildkite-test-collector' package to be installed.\nPlease install it with: pip install buildkite-test-collector.")
		}
		useJUnit = false
	}

	if useJUnit {
		fmt.Fprintln(os.Stderr, "Buildkite Test Engine Client: Using JUnit XML output for pytest results.")
	} else {
		fmt.Fprintln(os.Stderr, "Buildkite Test Engine Client: Using JSON output for pytest results.")
	}

	if c.TestFilePattern == "" {
		c.TestFilePattern = "**/{*_test,test_*}.py"
	}

	if c.RetryTestCommand == "" {
		c.RetryTestCommand = c.TestCommand
	}

	// Later operations use the same result format, even if they would detect
	// buildkite-test-collector differently.
	if request.StateDir != "" {
		if err := runnerplugin.SaveState(request.StateDir, stateFile, state{UseJUnit: useJUnit}); err != nil {
			return runnerplugin.ConfigureResponse{}, err
		}
	}

	response := runnerplugin.ConfigureResponse{
		Name:   "pytest",
		Config: c,
		Features: runnerplugin.Features{
			SplitByFile:     true,
			SplitByExample:  true,
			FilterTestFiles: true,
			FilterTestByTag: true,
			AutoRetry:       true,
			Mute:            true,
			Skip:            true,
			SplitBySelector: true,
		},
		// pytest's ResultFormat is for uploads, and is empty when
		// buildkite-test-collector uploads its JSON report itself.
		BatchReportFormat: "pytest-json",
	}
	if useJUnit {
		response.ResultFormat = "junit"
		response.BatchReportFormat = "pytest-junit"
	}
	return response, nil
}

// collectorInstalled reports whether pytest runs with buildkite-test-collector.
func collectorInstalled(c runnerplugin.Config) bool {
	// buildkite-pytest, pool exec's persistent runner, is part of
	// buildkite-test-collector and reports its JSON, whether or not bktec's
	// python can import it.
	return c.PersistentRunner || checkPythonPackageInstalled("buildkite_test_collector") // python import only uses underscore
}

// usesJUnit reports whether pytest's results are JUnit XML instead of
// buildkite-test-collector's JSON: what Configure decided, or, when an
// operation is called without it, what the test command says.
func usesJUnit(c runnerplugin.Config, stateDir string) (bool, error) {
	if stateDir != "" {
		var s state
		if found, err := runnerplugin.LoadState(stateDir, stateFile, &s); err != nil || found {
			return s.UseJUnit, err
		}
	}
	switch {
	case strings.Contains(c.TestCommand, "--junit-xml"):
		return true, nil
	case strings.Contains(c.TestCommand, "--json="):
		return false, nil
	default:
		return !collectorInstalled(c), nil
	}
}

func (handler) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error) {
	var response runnerplugin.InterpretAttemptResponse
	useJUnit, err := usesJUnit(request.Config, request.StateDir)
	if err != nil {
		return response, err
	}
	parseExit2JSON := false

	// Only rescue exit code 1 because it indicates a test failures.
	// Ref: https://docs.pytest.org/en/7.1.x/reference/exit-codes.html
	if exitCode := request.ExitCodeOr(0); exitCode != 0 && exitCode != 1 {
		// pytest exits 2 for collection errors, and buildkite-test-collector can
		// still write tagged JSON results for those errors.
		if useJUnit || exitCode != 2 {
			return response, nil
		}
		parseExit2JSON = true
	}

	var parseErr error
	if useJUnit {
		parseErr = runParseJUnit(&response, request.Config.ResultPath)
	} else {
		parseErr = runParseJSON(&response, request.Config.ResultPath)
	}
	if parseErr != nil {
		response.Messages = append(response.Messages, fmt.Sprintf("Buildkite Test Engine Client: Failed to read test output, failed tests will not be retried: %v", parseErr))
	}
	// Without a collection error in the report, the command's error is the
	// run's error.
	if parseExit2JSON && response.RunError == "" {
		response.RunError = request.Error
	}

	return response, nil
}

func runParseJSON(response *runnerplugin.InterpretAttemptResponse, resultPath string) error {
	tests, parseErr := runnerplugin.ReadTestEngineResults(resultPath)
	if parseErr != nil {
		return parseErr
	}

	for _, test := range tests {
		recordPytestJSONTestResult(response, test)
	}

	return nil
}

const pytestCollectionErrorTag = "test.pytest_collection_error"

func recordPytestJSONTestResult(response *runnerplugin.InterpretAttemptResponse, test runnerplugin.TestEngineTest) {
	path := pytestPathFromTestEngineResult(test.Scope, test.Name)

	response.Tests = append(response.Tests, runnerplugin.TestResult{
		TestCase: runnerplugin.TestCase{
			Identifier: test.ID,
			Format:     runnerplugin.FormatExample,
			Scope:      test.Scope,
			Name:       test.Name,
			// pytest can execute individual tests using node IDs, which are filenames,
			// class names (if any), and functions separated by `::`.
			// Ref: https://docs.pytest.org/en/6.2.x/usage.html#nodeids
			Path: path,
		},
		Status: test.Result,
	})

	if test.Tags[pytestCollectionErrorTag] == "true" {
		response.RunError = fmt.Sprintf("pytest collection failed: %s", path)
	}
}

func pytestPathFromTestEngineResult(scope, name string) string {
	if scope == "" {
		return name
	}

	return fmt.Sprintf("%s::%s", scope, name)
}

// ParseBatchReport parses the report of one pytest command run by bktec pool
// consume.
func (handler) ParseBatchReport(request runnerplugin.ParseBatchReportRequest) (runnerplugin.ParseBatchReportResponse, error) {
	var parse func([]byte) (runnerplugin.ParseBatchReportResponse, error)
	switch request.Format {
	case "pytest-json":
		parse = parsePytestJSONBatchReport
	case "pytest-junit":
		parse = parsePytestJUnitBatchReport
	default:
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("unsupported runner report format %q", request.Format)
	}
	data, err := os.ReadFile(request.ReportPath)
	if err != nil {
		return runnerplugin.ParseBatchReportResponse{}, err
	}
	return parse(data)
}

// parsePytestJSONBatchReport handles the buildkite-test-collector JSON report
// of one pytest command run by bktec pool consume.
func parsePytestJSONBatchReport(data []byte) (runnerplugin.ParseBatchReportResponse, error) {
	var tests *[]runnerplugin.TestEngineTest
	if err := json.Unmarshal(data, &tests); err != nil {
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("invalid pytest JSON report: %w", err)
	}
	if tests == nil {
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("invalid pytest JSON report: not a list of tests")
	}
	var parsed runnerplugin.ParseBatchReportResponse
	seen := map[string]int{}
	for _, test := range *tests {
		// A file that fails to collect has no tests to report, so its attempt
		// errors. Other files keep their results: buildkite-pytest and
		// --continue-on-collection-errors run them anyway, and pool consume
		// already errors the batch when pytest stops at a collection error.
		if test.Tags[pytestCollectionErrorTag] == "true" {
			// An error outside a leased file, such as a package directory's
			// conftest.py, matches no dispatched file and errors the batch.
			file, _, _ := strings.Cut(pytestPathFromTestEngineResult(test.Scope, test.Name), "::")
			if !slices.Contains(parsed.ErroredFiles, file) {
				parsed.ErroredFiles = append(parsed.ErroredFiles, file)
			}
			continue
		}
		path := pytestPathFromTestEngineResult(test.Scope, test.Name)
		if err := addPytestBatchTest(&parsed, seen, runnerplugin.TestCase{Scope: test.Scope, Name: test.Name, Path: path}, test.Result); err != nil {
			return runnerplugin.ParseBatchReportResponse{}, err
		}
	}
	return parsed, nil
}

// parsePytestJUnitBatchReport handles the JUnit XML report of one pytest
// command run by bktec pool consume.
func parsePytestJUnitBatchReport(data []byte) (runnerplugin.ParseBatchReportResponse, error) {
	tests, err := runnerplugin.ParseJUnitXML(data)
	if err != nil {
		return runnerplugin.ParseBatchReportResponse{}, fmt.Errorf("invalid pytest JUnit XML report: %w", err)
	}
	var parsed runnerplugin.ParseBatchReportResponse
	seen := map[string]int{}
	for _, test := range tests {
		// pytest reports collection errors and module-level skips without a
		// classname. Neither is a test.
		if test.Classname == "" {
			if test.Result != runnerplugin.StatusSkipped {
				parsed.ErrorsOutsideTests = true
			}
			continue
		}
		// Scope is the raw classname, as in runParseJUnit.
		path := pytestNodeIDFromJUnit(test.Classname, test.Name)
		if err := addPytestBatchTest(&parsed, seen, runnerplugin.TestCase{Scope: test.Classname, Name: test.Name, Path: path}, test.Result); err != nil {
			return runnerplugin.ParseBatchReportResponse{}, err
		}
	}
	return parsed, nil
}

// addPytestBatchTest reports a test by its node ID, which pytest reports
// relative to its rootdir, and matches it to the leased file in the node ID.
// pytest can report a test twice, such as when it fails and its teardown
// errors, so the test is reported once: failed if any report failed.
func addPytestBatchTest(parsed *runnerplugin.ParseBatchReportResponse, seen map[string]int, test runnerplugin.TestCase, status string) error {
	switch status {
	case runnerplugin.StatusPassed, runnerplugin.StatusFailed, runnerplugin.StatusSkipped:
	default:
		return fmt.Errorf("unknown pytest test status %q", status)
	}
	file, _, _ := strings.Cut(test.Path, "::")
	if file == "" || test.Name == "" {
		return fmt.Errorf("pytest test %q has no file or name", test.Path)
	}
	if i, ok := seen[test.Path]; ok {
		if status == runnerplugin.StatusFailed || parsed.Tests[i].Status == runnerplugin.StatusSkipped {
			parsed.Tests[i].Status = status
		}
		return nil
	}
	test.Identifier = test.Path
	seen[test.Path] = len(parsed.Tests)
	parsed.Tests = append(parsed.Tests, runnerplugin.BatchTest{TestCase: test, Status: status, Selector: file})
	return nil
}

func runParseJUnit(response *runnerplugin.InterpretAttemptResponse, resultPath string) error {
	tests, parseErr := runnerplugin.ReadJUnitXML(resultPath)
	if parseErr != nil {
		return parseErr
	}

	for _, test := range tests {
		path := pytestNodeIDFromJUnit(test.Classname, test.Name)
		response.Tests = append(response.Tests, runnerplugin.TestResult{
			TestCase: runnerplugin.TestCase{
				Identifier: path,
				Format:     runnerplugin.FormatExample,
				// JUnit XML ingestion set Scope to the raw classname
				Scope: test.Classname,
				Name:  test.Name,
				Path:  path,
			},
			Status: test.Result,
		})
	}

	return nil
}

// pytestNodeIDFromJUnit reconstructs a pytest node ID (scope and path) from a JUnit XML
// classname and test name. pytest encodes the module path in the classname using dots as
// separators, with any class names appended after the module.
//
// Examples:
//
//	classname="test_sample",                          name="test_happy"   → scope="test_sample.py",                          path="test_sample.py::test_happy"
//	classname="tests.test_sample",                    name="test_happy"   → scope="tests/test_sample.py",                    path="tests/test_sample.py::test_happy"
//	classname="tests.test_auth.TestLogin",            name="test_success" → scope="tests/test_auth.py::TestLogin",           path="tests/test_auth.py::TestLogin::test_success"
//	classname="tests.MyTest.test_subtract.TestClass", name="test_positive"→ scope="tests/MyTest/test_subtract.py::TestClass",path="tests/MyTest/test_subtract.py::TestClass::test_positive"
//	classname="tests.MyTest.test_add",                name="test_add"     → scope="tests/MyTest/test_add.py",                path="tests/MyTest/test_add.py::test_add"
func pytestNodeIDFromJUnit(classname, name string) (path string) {
	if classname == "" {
		return name
	}

	parts := strings.Split(classname, ".")

	// Identify the module (test file) as the last component that matches pytest's test file
	// naming convention (test_* or *_test). This correctly handles uppercase package directories
	// such as tests.MyTest.test_foo.TestClass, where MyTest is a directory, not a class.
	// If no such component is found (non-standard naming), fall back to stopping at the first
	// CamelCase component.
	moduleEnd := -1
	for i, p := range parts {
		if strings.HasPrefix(p, "test_") || strings.HasSuffix(p, "_test") {
			moduleEnd = i + 1
		}
	}

	if moduleEnd == -1 {
		moduleEnd = len(parts)
		for i, p := range parts {
			if len(p) > 0 && p[0] >= 'A' && p[0] <= 'Z' {
				moduleEnd = i
				break
			}
		}
	}

	modulePath := strings.Join(parts[:moduleEnd], "/") + ".py"
	classParts := parts[moduleEnd:]

	if len(classParts) == 0 {
		path = modulePath + "::" + name
	} else {
		classPath := strings.Join(classParts, "::")
		path = modulePath + "::" + classPath + "::" + name
	}
	return
}

// DiscoverExamples returns an array of test examples within the given files.
// It uses `pytest --collect-only -q` to enumerate individual tests.
//
// --tag-filters can be used to filter tests by markers if specified.
// e.g. --tag-fitlers team:frontend matches markers:
// with @pytest.mark.execution_tag('team', 'frontend')
//
// The --tag-filters feature also assumes Python Test Collector plugin
// version >1.2.0 is installed.
func (handler) DiscoverExamples(request runnerplugin.DiscoverExamplesRequest) (runnerplugin.DiscoverExamplesResponse, error) {
	response := runnerplugin.DiscoverExamplesResponse{TestCases: []runnerplugin.TestCase{}}
	if len(request.Files) == 0 {
		return response, nil
	}
	useJUnit, err := usesJUnit(request.Config, request.StateDir)
	if err != nil {
		return response, err
	}

	args := []string{"--collect-only", "-q"}
	if request.Config.TagFilters != "" {
		args = append(args, "--tag-filters", request.Config.TagFilters)
	}
	args = append(args, request.Files...)
	// #nosec G204 -- the files and tag filters are configured by the bktest user.
	cmd := exec.Command("pytest", args...)

	output, err := cmd.Output()
	if err != nil {
		// Include stderr in error message for debugging
		if exitErr, ok := err.(*exec.ExitError); ok {
			return response, fmt.Errorf("pytest collection failed: %s", exitErr.Stderr)
		}
		return response, fmt.Errorf("pytest collection failed: %w", err)
	}

	testCases, err := parsePytestCollectOutput(string(output), useJUnit)
	if err != nil {
		return response, err
	}
	response.TestCases = testCases
	return response, nil
}

// parsePytestCollectOutput parses the output of `pytest --collect-only -q`
// and returns a list of test cases.
//
// Example output:
//
//	test_sample.py::test_happy
//	test_auth.py::TestLogin::test_success
//	test_auth.py::test_param[value1]
//
//	3 tests collected in 0.05s
func parsePytestCollectOutput(output string, useJUnit bool) ([]runnerplugin.TestCase, error) {
	var testCases []runnerplugin.TestCase

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)

		// Skip empty lines and summary lines (lines without ::)
		if line == "" || !strings.Contains(line, "::") {
			continue
		}

		// Parse node ID: "file.py::TestClass::test_method" or "file.py::test_func"
		testCases = append(testCases, mapNodeIDToTestCase(line, useJUnit))
	}

	return testCases, nil
}

// mapNodeIDToTestCase converts a pytest node ID to a TestCase.
// Node ID format: file_path::class::method or file_path::function
// Must match the format used by buildkite-test-collector for pytest.
// Scope is everything except the final component, Name is the last component.
func mapNodeIDToTestCase(nodeID string, useJUnit bool) runnerplugin.TestCase {
	// Split on the last :: to get scope (everything before) and name (last component)
	lastIdx := strings.LastIndex(nodeID, "::")
	scope := ""
	name := nodeID
	if lastIdx != -1 {
		scope = nodeID[:lastIdx]
		name = nodeID[lastIdx+2:]
	}

	// There is difference between the scope reported by test-collector and native JUnit XML.
	if useJUnit {
		scope = collectorScopeToJunitScope(scope)
	}

	return runnerplugin.TestCase{
		Identifier: nodeID,
		Path:       nodeID,
		Scope:      scope,
		Name:       name,
		Format:     runnerplugin.FormatExample,
	}
}

// collectorScopeToJunitScope converts a pytest scope (from test-collector) to a JUnit scope (from JUnit XML).
// test-collector reports the scope as the file path (e.g. tests/test_auth.py::TestLogin),
// while JUnit XML reports the scope as the raw classname (e.g. tests.test_auth.TestLogin).
// Strip the `.py` extension from the file path, then replace `::` and `/` with `.`.
// The file path is always the part before the first `::`, so we only trim the suffix on
// that part. Doing so avoids mangling a directory that happens to end in `.py`.
// Example:
//
//	test_sample.py -> test_sample
//	tests/test_auth.py::TestLogin -> tests.test_auth.TestLogin
//	tests/MyTest/test_subtract.py::TestClass -> tests.MyTest.test_subtract.TestClass
//	tests/test_auth.py::TestOuter::TestInner -> tests.test_auth.TestOuter.TestInner
//	tests/pkg.py/test_auth.py::TestLogin -> tests.pkg.py.test_auth.TestLogin
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

func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	cmd := request.Config.TestCommand
	if request.Retry {
		cmd = request.Config.RetryTestCommand
	}

	testPaths := runnerplugin.Paths(request.TestCases)

	testExamples := shellquote.Join(testPaths...)

	if strings.Contains(cmd, "{{testExamples}}") {
		cmd = strings.Replace(cmd, "{{testExamples}}", testExamples, 1)
	} else {
		cmd = cmd + " " + testExamples
	}

	cmd = strings.Replace(cmd, "{{resultPath}}", request.Config.ResultPath, 1)

	args, err := shellquote.Split(cmd)
	if err != nil {
		return runnerplugin.PrepareAttemptResponse{}, err
	}

	return runnerplugin.PrepareAttemptResponse{Argv: args}, nil
}

func getRandomTempFilename(filename string) (string, error) {
	tempDir, err := os.MkdirTemp("", "bktec-pytest-*")
	if err != nil {
		return "", err
	}
	return filepath.Join(tempDir, filename), nil
}

// getPythonPackageVersion retrieves the version of a Python package using importlib.metadata.
// The pkgName should use hyphens (e.g., "buildkite-test-collector") as that's the package name in metadata.
func getPythonPackageVersion(pkgName string) (string, error) {
	// #nosec G204 -- the package name is a constant in this file.
	pythonCmd := exec.Command("python", "-c", "import importlib.metadata, sys; print(importlib.metadata.version(sys.argv[1]))", pkgName)
	output, err := pythonCmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not determine %s version: %w", pkgName, err)
	}

	return strings.TrimSpace(string(output)), nil
}

// checkBuildkiteTestCollectorVersion verifies that the installed buildkite-test-collector
// version is greater than the specified required version.
func checkBuildkiteTestCollectorVersion(requiredVersion string) error {
	installedVersionStr, err := getPythonPackageVersion("buildkite-test-collector")
	if err != nil {
		return err
	}

	// semver package requires versions to be prefixed with "v"
	installedVersionCanonical := "v" + installedVersionStr
	requiredVersionCanonical := "v" + requiredVersion

	if !semver.IsValid(installedVersionCanonical) {
		return fmt.Errorf("could not parse installed buildkite-test-collector version %q", installedVersionStr)
	}

	if !semver.IsValid(requiredVersionCanonical) {
		return fmt.Errorf("could not parse required version %q", requiredVersion)
	}

	// semver.Compare returns -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
	// We want installed > required, so compare should return 1
	if semver.Compare(installedVersionCanonical, requiredVersionCanonical) <= 0 {
		return fmt.Errorf("buildkite-test-collector version %s is installed, but version >%s is required for --tag-filters support", installedVersionStr, requiredVersion)
	}

	return nil
}

func checkPythonPackageInstalled(pkgName string) bool {
	// This is the most reliable way I can find. Hopefully it should work regardless of if user uses pip, poetry or uv
	// #nosec G204 -- the package name is a constant in this file.
	pythonCmd := exec.Command("python", "-c", "import importlib.util, sys; print(importlib.util.find_spec(sys.argv[1]) is not None)", pkgName)
	output, err := pythonCmd.Output()

	return err == nil && strings.TrimSpace(string(output)) == "True"
}
