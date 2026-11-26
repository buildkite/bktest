package runner

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
)

// requirePythonPackage skips the test unless python can import the package.
func requirePythonPackage(t *testing.T, name string) {
	t.Helper()
	if err := exec.Command("python", "-c", "import "+name).Run(); err != nil {
		t.Skipf("the %s Python package is not installed", name)
	}
}

// Testing happy path where all configurtions are auto configured.
func TestPytestRun(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{})
	testCases := []plan.TestCase{
		{Path: "test_sample.py"},
	}
	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, false)
	if err != nil {
		t.Errorf("Pytest.Run(%v) error = %v", testCases, err)
	}

	if result.Status() != RunStatusPassed {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
	}
}

func TestPytestRun_RetryCommand(t *testing.T) {
	// Without buildkite-test-collector, pytest results are JUnit XML.
	requirePythonPackage(t, "buildkite_test_collector")
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{
		TestCommand:      "pytest failed_test.py",
		RetryTestCommand: "pytest",
		ResultPath:       "result-passed.json",
	})

	testCases := []plan.TestCase{
		{Path: "test_sample.py"},
	}

	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, true)
	if err != nil {
		t.Errorf("Pytest.Run(%v) error = %v", testCases, err)
	}
}

func TestPytestRun_TestFailed(t *testing.T) {
	// Without buildkite-test-collector, pytest results are JUnit XML.
	requirePythonPackage(t, "buildkite_test_collector")
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{
		TestCommand: "pytest",
		ResultPath:  "result-failed.json",
	})
	testCases := []plan.TestCase{
		{Path: "failed_test.py"},
	}
	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, false)

	exitError := new(exec.ExitError)
	assert.ErrorAs(t, err, &exitError)

	if result.Status() != RunStatusFailed {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusFailed)
	}

	failedTest := result.FailedTests()

	if len(failedTest) != 1 {
		t.Errorf("len(result.FailedTests()) = %d, want 1", len(failedTest))
	}

	wantFailedTests := []plan.TestCase{
		{
			Format:     "example",
			Identifier: "a1be7e52-0dba-4018-83ce-a1598ca68807",
			Name:       "test_failed",
			Path:       "tests/failed_test.py::test_failed",
			Scope:      "tests/failed_test.py",
		},
	}

	if diff := cmp.Diff(failedTest, wantFailedTests); diff != "" {
		t.Errorf("Pytest.Run(%v) RunResult.FailedTests() diff (-got +want):\n%s", testCases, diff)
	}
}

func TestPytestRun_CollectionError(t *testing.T) {
	requirePythonPackage(t, "pytest")
	requirePythonPackage(t, "buildkite_test_collector")

	changeCwd(t, "./testdata/pytest_collection_error")

	resultPath := filepath.Join(t.TempDir(), "result.json")
	pytest := NewPytest(RunnerConfig{
		TestCommand: "python -m pytest {{testExamples}} --json={{resultPath}}",
		ResultPath:  resultPath,
	})
	testCases := []plan.TestCase{{Path: "test_broken_import.py"}}
	result := NewRunResult([]plan.TestCase{})

	err := pytest.Run(result, testCases, false)

	exitError := new(exec.ExitError)
	if !assert.ErrorAs(t, err, &exitError) {
		return
	}
	if exitError.ExitCode() != 2 {
		t.Errorf("Pytest.Run(%v) exit code = %d, want 2", testCases, exitError.ExitCode())
	}

	if result.Status() != RunStatusError {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusError)
	}
	if result.Error() == nil {
		t.Fatalf("Pytest.Run(%v) RunResult.Error = nil, want error", testCases)
	}
	if got, want := result.Error().Error(), "pytest collection failed: test_broken_import.py"; got != want {
		t.Errorf("Pytest.Run(%v) RunResult.Error = %q, want %q", testCases, got, want)
	}

	failedTests := result.FailedTests()
	if len(failedTests) != 1 {
		t.Fatalf("len(result.FailedTests()) = %d, want 1", len(failedTests))
	}
	failedTest := failedTests[0]
	if failedTest.Scope != "" {
		t.Errorf("Pytest.Run(%v) failed test scope = %q, want empty", testCases, failedTest.Scope)
	}
	if failedTest.Name != "test_broken_import.py" {
		t.Errorf("Pytest.Run(%v) failed test name = %q, want %q", testCases, failedTest.Name, "test_broken_import.py")
	}
	if failedTest.Path != "test_broken_import.py" {
		t.Errorf("Pytest.Run(%v) failed test path = %q, want %q", testCases, failedTest.Path, "test_broken_import.py")
	}
}

func TestPytestRun_JSONExit2ParsesCollectionError(t *testing.T) {
	// Without buildkite-test-collector, pytest results are JUnit XML.
	requirePythonPackage(t, "buildkite_test_collector")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	json := `[
		{
			"id": "collection-error-id",
			"scope": "",
			"name": "tests/test_broken.py",
			"result": "failed",
			"tags": {"test.pytest_collection_error": "true"}
		}
	]`
	pytest := NewPytest(RunnerConfig{
		TestCommand: "sh -c 'printf %s \"$1\" > \"$2\"; exit 2' sh " + shellquote.Join(json) + " {{resultPath}}",
		ResultPath:  resultPath,
	})
	result := NewRunResult([]plan.TestCase{})

	err := pytest.Run(result, nil, false)

	exitError := new(exec.ExitError)
	if !assert.ErrorAs(t, err, &exitError) {
		return
	}
	if exitError.ExitCode() != 2 {
		t.Errorf("Pytest.Run() exit code = %d, want 2", exitError.ExitCode())
	}
	if result.Status() != RunStatusError {
		t.Errorf("Pytest.Run() RunResult.Status = %v, want %v", result.Status(), RunStatusError)
	}
	if result.Error() == nil {
		t.Fatal("Pytest.Run() RunResult.Error = nil, want error")
	}
	if got, want := result.Error().Error(), "pytest collection failed: tests/test_broken.py"; got != want {
		t.Errorf("Pytest.Run() RunResult.Error = %q, want %q", got, want)
	}
}

func TestPytestRun_JSONExit2WithoutCollectionErrorIsTerminal(t *testing.T) {
	// Without buildkite-test-collector, pytest results are JUnit XML.
	requirePythonPackage(t, "buildkite_test_collector")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	json := `[
		{
			"id": "failed-test-id",
			"scope": "tests/test_sample.py",
			"name": "test_failed",
			"result": "failed"
		}
	]`
	pytest := NewPytest(RunnerConfig{
		TestCommand: "sh -c 'printf %s \"$1\" > \"$2\"; exit 2' sh " + shellquote.Join(json) + " {{resultPath}}",
		ResultPath:  resultPath,
	})
	result := NewRunResult([]plan.TestCase{})

	err := pytest.Run(result, nil, false)

	exitError := new(exec.ExitError)
	if !assert.ErrorAs(t, err, &exitError) {
		return
	}
	if exitError.ExitCode() != 2 {
		t.Errorf("Pytest.Run() exit code = %d, want 2", exitError.ExitCode())
	}
	if result.Status() != RunStatusError {
		t.Errorf("Pytest.Run() RunResult.Status = %v, want %v", result.Status(), RunStatusError)
	}
	resultExitError := new(exec.ExitError)
	if !assert.ErrorAs(t, result.Error(), &resultExitError) {
		return
	}
	if resultExitError.ExitCode() != 2 {
		t.Errorf("Pytest.Run() RunResult.Error exit code = %d, want 2", resultExitError.ExitCode())
	}
}

func TestPytestRun_JUnit_TestPassed(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{
		// --junit-xml makes pytest results JUnit XML. The report that is
		// read is the fixture at the result path.
		TestCommand: "pytest --junit-xml=/dev/null",
		ResultPath:  "result-passed.xml",
	})
	testCases := []plan.TestCase{{Path: "test_sample.py"}}
	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, false)
	if err != nil {
		t.Errorf("Pytest.Run(%v) error = %v", testCases, err)
	}

	if result.Status() != RunStatusPassed {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
	}

	var passedTests []plan.TestCase
	for _, tr := range result.tests {
		if tr.Status == TestStatusPassed {
			passedTests = append(passedTests, tr.TestCase)
		}
	}
	wantPassedTests := []plan.TestCase{
		{
			Format:     "example",
			Identifier: "test_sample.py::test_happy",
			Name:       "test_happy",
			Path:       "test_sample.py::test_happy",
			Scope:      "test_sample",
		},
	}
	if diff := cmp.Diff(passedTests, wantPassedTests); diff != "" {
		t.Errorf("Pytest.Run(%v) passed tests diff (-got +want):\n%s", testCases, diff)
	}
}

func TestPytestRun_JUnit_TestFailed(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{
		// --junit-xml makes pytest results JUnit XML. The report that is
		// read is the fixture at the result path.
		TestCommand: "pytest --junit-xml=/dev/null",
		ResultPath:  "result-failed.xml",
	})
	testCases := []plan.TestCase{{Path: "failed_test.py"}}
	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, false)

	exitError := new(exec.ExitError)
	assert.ErrorAs(t, err, &exitError)

	if result.Status() != RunStatusFailed {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusFailed)
	}

	failedTest := result.FailedTests()
	if len(failedTest) != 1 {
		t.Errorf("len(result.FailedTests()) = %d, want 1", len(failedTest))
	}

	wantFailedTests := []plan.TestCase{
		{
			Format:     "example",
			Identifier: "tests/failed_test.py::test_failed",
			Name:       "test_failed",
			Path:       "tests/failed_test.py::test_failed",
			Scope:      "tests.failed_test",
		},
	}

	if diff := cmp.Diff(failedTest, wantFailedTests); diff != "" {
		t.Errorf("Pytest.Run(%v) RunResult.FailedTests() diff (-got +want):\n%s", testCases, diff)
	}
}

func TestPytestRun_TestFailedWithoutResultFile(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	// When there is TestCommand, but it didn't leave space for ResultPath
	pytest := NewPytest(RunnerConfig{
		TestCommand: "pytest",
	})
	testCases := []plan.TestCase{
		{Path: "failed_test.py"},
	}
	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, false)

	if result.Status() != RunStatusUnknown {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
	}

	exitError := new(exec.ExitError)
	assert.ErrorAs(t, err, &exitError)
}

func TestPytestRun_CommandFailed(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{
		TestCommand: "pytest help",
	})

	testCases := []plan.TestCase{
		{Path: "test_sample.py"},
	}
	result := NewRunResult([]plan.TestCase{})
	err := pytest.Run(result, testCases, false)

	if result.Status() != RunStatusUnknown {
		t.Errorf("Pytest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
	}

	exitError := new(exec.ExitError)
	assert.ErrorAs(t, err, &exitError)
}

func TestPytestDiscoverTestTargets(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{})

	got, err := pytest.DiscoverTestTargets()
	if err != nil {
		t.Errorf("Pytest.DiscoverTestTargets() error = %v", err)
	}

	want := []string{
		"failed_test.py",
		"spells/test_expelliarmus.py",
		"test_sample.py",
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("Pytest.DiscoverTestTargets() diff (-got +want):\n%s", diff)
	}
}

func TestPytestCommandNameAndArgs_WithInterpolationPlaceholder(t *testing.T) {
	testCases := []plan.TestCase{{Path: "failed_test.py"}, {Path: "test_sample.py"}}
	testCommand := "pytest {{testExamples}} --full-trace --json={{resultPath}}"

	pytest := NewPytest(RunnerConfig{
		TestCommand: testCommand,
		ResultPath:  "result.json",
	})

	gotName, gotArgs, err := pytest.CommandNameAndArgs(testCases, false)
	if err != nil {
		t.Errorf("commandNameAndArgs(%v, %q) error = %v", testCases, testCommand, err)
	}

	wantName := "pytest"
	wantArgs := []string{"failed_test.py", "test_sample.py", "--full-trace", "--json=result.json"}

	if diff := cmp.Diff(gotName, wantName); diff != "" {
		t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
	}
	if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
		t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
	}
}

func TestPytestCommandNameAndArgs_WithoutTestExamplesPlaceholder(t *testing.T) {
	testCases := []plan.TestCase{{Path: "failed_test.py"}, {Path: "test_sample.py"}}
	testCommand := "pytest --full-trace"

	pytest := NewPytest(RunnerConfig{
		TestCommand: testCommand,
	})

	gotName, gotArgs, err := pytest.CommandNameAndArgs(testCases, false)
	if err != nil {
		t.Errorf("commandNameAndArgs(%v, %q) error = %v", testCases, testCommand, err)
	}

	wantName := "pytest"
	wantArgs := []string{"--full-trace", "failed_test.py", "test_sample.py"}

	if diff := cmp.Diff(gotName, wantName); diff != "" {
		t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
	}
	if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
		t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
	}
}

func TestPytestCommandNameAndArgs_InvalidTestCommand(t *testing.T) {
	testCases := []plan.TestCase{{Path: "failed_test.py"}, {Path: "test_sample.py"}}
	testCommand := "pytest '{{testExamples}}"

	pytest := NewPytest(RunnerConfig{
		TestCommand: testCommand,
	})

	gotName, gotArgs, err := pytest.CommandNameAndArgs(testCases, false)

	wantName := ""
	wantArgs := []string{}

	if diff := cmp.Diff(gotName, wantName); diff != "" {
		t.Errorf("commandNameAndArgs() diff (-got +want):\n%s", diff)
	}
	if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
		t.Errorf("commandNameAndArgs() diff (-got +want):\n%s", diff)
	}
	if !errors.Is(err, shellquote.UnterminatedSingleQuoteError) {
		t.Errorf("commandNameAndArgs() error = %v, want %v", err, shellquote.UnterminatedSingleQuoteError)
	}
}

func TestPytestGetExamples(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(RunnerConfig{})
	files := []string{"spells/test_expelliarmus.py"}
	got, err := pytest.GetExamples(files)
	if err != nil {
		t.Fatalf("Pytest.GetExamples(%q) error = %v", files, err)
	}

	want := []plan.TestCase{
		{
			Identifier: "spells/test_expelliarmus.py::TestExpelliarmus::test_disarms_opponent",
			Name:       "test_disarms_opponent",
			Path:       "spells/test_expelliarmus.py::TestExpelliarmus::test_disarms_opponent",
			Scope:      "spells/test_expelliarmus.py::TestExpelliarmus",
			Format:     plan.TestCaseFormatExample,
		},
		{
			Identifier: "spells/test_expelliarmus.py::TestExpelliarmus::test_knocks_wand_out",
			Name:       "test_knocks_wand_out",
			Path:       "spells/test_expelliarmus.py::TestExpelliarmus::test_knocks_wand_out",
			Scope:      "spells/test_expelliarmus.py::TestExpelliarmus",
			Format:     plan.TestCaseFormatExample,
		},
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("Pytest.GetExamples(%q) diff (-got +want):\n%s", files, diff)
	}
}

func TestPytestGetExamples_EmptyFiles(t *testing.T) {
	pytest := NewPytest(RunnerConfig{})
	got, err := pytest.GetExamples([]string{})
	if err != nil {
		t.Errorf("Pytest.GetExamples([]) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pytest.GetExamples([]) = %v, want empty slice", got)
	}
}

func TestPytestGetExamples_TagFilter(t *testing.T) {
	changeCwd(t, "./testdata/pytest")

	pytest := NewPytest(
		RunnerConfig{
			TagFilters: "team:frontend",
		},
	)

	files, _ := pytest.DiscoverTestTargets()

	got, err := pytest.GetExamples(files)
	if err != nil {
		t.Fatalf("Pytest.GetExamples(%q) error = %v", files, err)
	}

	if len(got) != 2 {
		t.Fatalf("Pytest.GetExamples(%q) with tag filter 'team:frontend' returned %d tests, want 2", files, len(got))
	}

	if got[0].Name != "test_knocks_wand_out" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "test_knocks_wand_out")
	}

	if got[1].Name != "test_happy" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "test_happy")
	}
}

func TestPytestResultFormat_JUnit(t *testing.T) {
	pytest := NewPytest(RunnerConfig{TestCommand: "pytest {{testExamples}} --junit-xml={{resultPath}}"})
	if got := pytest.ResultFormat(); got != "junit" {
		t.Errorf("ResultFormat() = %q, want %q", got, "junit")
	}
}

func TestPytestResultFormat_JSON(t *testing.T) {
	requirePythonPackage(t, "buildkite_test_collector")
	pytest := NewPytest(RunnerConfig{})
	if got := pytest.ResultFormat(); got != "" {
		t.Errorf("ResultFormat() = %q, want %q (empty, test collector handles upload)", got, "")
	}
}
