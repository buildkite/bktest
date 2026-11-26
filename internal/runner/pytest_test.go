package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPytestRun_JSONExit2ParsesCollectionError(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		resultPath := filepath.Join(t.TempDir(), "result.json")
		jsonBody := `[
			{
				"id": "collection-error-id",
				"scope": "",
				"name": "tests/test_broken.py",
				"result": "failed",
				"tags": {"test.pytest_collection_error": "true"}
			}
		]`
		pytest := loadVariantRunner(t, variant, "pytest", RunnerConfig{
			TestCommand:      "sh -c 'printf %s \"$1\" > \"$2\"; exit 2' sh " + shellquote.Join(jsonBody) + " {{resultPath}}",
			ResultPath:       resultPath,
			persistentRunner: true,
		})
		result := NewRunResult([]plan.TestCase{})

		err := pytest.Run(result, nil, false)
		exitError := new(exec.ExitError)
		require.ErrorAs(t, err, &exitError)
		assert.Equal(t, 2, exitError.ExitCode())
		assert.Equal(t, RunStatusError, result.Status())
		require.Error(t, result.Error())
		assert.Equal(t, "pytest collection failed: tests/test_broken.py", result.Error().Error())
	})
}

func TestPytestRun_JSONExit2WithoutCollectionErrorIsTerminal(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		resultPath := filepath.Join(t.TempDir(), "result.json")
		jsonBody := `[
			{
				"id": "failed-test-id",
				"scope": "tests/test_sample.py",
				"name": "test_failed",
				"result": "failed"
			}
		]`
		pytest := loadVariantRunner(t, variant, "pytest", RunnerConfig{
			TestCommand:      "sh -c 'printf %s \"$1\" > \"$2\"; exit 2' sh " + shellquote.Join(jsonBody) + " {{resultPath}}",
			ResultPath:       resultPath,
			persistentRunner: true,
		})
		result := NewRunResult([]plan.TestCase{})

		err := pytest.Run(result, nil, false)
		exitError := new(exec.ExitError)
		require.ErrorAs(t, err, &exitError)
		assert.Equal(t, 2, exitError.ExitCode())
		assert.Equal(t, RunStatusError, result.Status())

		resultExitError := new(exec.ExitError)
		require.ErrorAs(t, result.Error(), &resultExitError)
		assert.Equal(t, 2, resultExitError.ExitCode())
	})
}

func TestPytestRun_JUnitAndJSONResults(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/pytest")

		// JUnit passed
		pytestJUnit := loadVariantRunner(t, variant, "pytest", RunnerConfig{
			TestCommand: "true --junit-xml={{resultPath}}",
			ResultPath:  "result-passed.xml",
		})
		res := NewRunResult(nil)
		require.NoError(t, pytestJUnit.Run(res, []plan.TestCase{{Path: "test_sample.py"}}, false))
		assert.Equal(t, RunStatusPassed, res.Status())
		assert.Equal(t, "junit", pytestJUnit.ResultFormat())

		var passedTests []plan.TestCase
		for _, tr := range res.tests {
			if tr.Status == TestStatusPassed {
				passedTests = append(passedTests, tr.TestCase)
			}
		}
		wantPassed := []plan.TestCase{{
			Format:     plan.TestCaseFormatExample,
			Identifier: "test_sample.py::test_happy",
			Name:       "test_happy",
			Path:       "test_sample.py::test_happy",
			Scope:      "test_sample",
		}}
		if diff := cmp.Diff(passedTests, wantPassed); diff != "" {
			t.Errorf("JUnit passed tests diff (-got +want):\n%s", diff)
		}

		// JSON failed
		pytestJSON := loadVariantRunner(t, variant, "pytest", RunnerConfig{
			TestCommand:      "sh -c 'exit 1'",
			ResultPath:       "result-failed.json",
			persistentRunner: true,
		})
		resFail := NewRunResult(nil)
		err := pytestJSON.Run(resFail, []plan.TestCase{{Path: "failed_test.py"}}, false)
		assert.ErrorAs(t, err, new(*exec.ExitError))
		assert.Equal(t, RunStatusFailed, resFail.Status())
		assert.Equal(t, "", pytestJSON.ResultFormat())
		wantFailed := []plan.TestCase{{
			Format:     plan.TestCaseFormatExample,
			Identifier: "a1be7e52-0dba-4018-83ce-a1598ca68807",
			Name:       "test_failed",
			Path:       "tests/failed_test.py::test_failed",
			Scope:      "tests/failed_test.py",
		}}
		if diff := cmp.Diff(resFail.FailedTests(), wantFailed); diff != "" {
			t.Errorf("JSON failed tests diff (-got +want):\n%s", diff)
		}
	})
}

func TestPytestCommandNameAndArgs_ParametrizedWithSpaces(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		testCases := []plan.TestCase{
			{Path: "foo/bar.py::TestFoo::test_foo[min-WeightedScalar-valid_reduce_ops0-only sum or avg are supported-2]"},
			{Path: "test_sample.py::test_simple"},
		}
		pytest := loadVariantRunner(t, variant, "pytest", RunnerConfig{
			TestCommand:      "pytest {{testExamples}} --json={{resultPath}}",
			ResultPath:       "result.json",
			persistentRunner: true,
		})
		gotName, gotArgs, err := pytest.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "pytest", gotName)
		assert.Equal(t, []string{
			"foo/bar.py::TestFoo::test_foo[min-WeightedScalar-valid_reduce_ops0-only sum or avg are supported-2]",
			"test_sample.py::test_simple",
			"--json=result.json",
		}, gotArgs)
	})
}

func TestPytestDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/pytest")
		pytest := loadVariantRunner(t, variant, "pytest", RunnerConfig{})
		got, err := pytest.DiscoverTestTargets()
		require.NoError(t, err)
		want := []string{
			"failed_test.py",
			"spells/test_expelliarmus.py",
			"test_sample.py",
		}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestPytestGetExamples_EmptyFiles(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		pytest := loadVariantExampleRunner(t, variant, "pytest", RunnerConfig{})
		got, err := pytest.GetExamples([]string{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestParseNativePytestReports(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		jsonData := []byte(`[
			{"id":"1","scope":"tests/test_a.py::TestA","name":"test_one","result":"passed"},
			{"id":"2","scope":"","name":"tests/test_broken.py","result":"failed","tags":{"test.pytest_collection_error":"true"}}
		]`)
		gotJSON, err := ParseNativeReportWithOptions("pytest-json", jsonData, nil, variant)
		require.NoError(t, err)
		assert.Equal(t, []string{"tests/test_broken.py"}, gotJSON.ErroredFiles)
		require.Len(t, gotJSON.Tests, 1)
		assert.Equal(t, "tests/test_a.py::TestA::test_one", gotJSON.Tests[0].TestCase.Path)
		assert.Equal(t, "tests/test_a.py", gotJSON.Tests[0].Selector)

		junitData := []byte(`<testsuites><testsuite name="pytest">
			<testcase classname="tests.MyTest.test_subtract.TestSubtract" name="test_positive"/>
			<testcase classname="" name="collection_err"><failure message="boom">err</failure></testcase>
		</testsuite></testsuites>`)
		gotJUnit, err := ParseNativeReportWithOptions("pytest-junit", junitData, nil, variant)
		require.NoError(t, err)
		assert.True(t, gotJUnit.ErrorsOutsideTests)
		require.Len(t, gotJUnit.Tests, 1)
		assert.Equal(t, "tests/MyTest/test_subtract.py::TestSubtract::test_positive", gotJUnit.Tests[0].TestCase.Path)
		assert.Equal(t, "tests.MyTest.test_subtract.TestSubtract", gotJUnit.Tests[0].TestCase.Scope)
		assert.Equal(t, "tests/MyTest/test_subtract.py", gotJUnit.Tests[0].Selector)
	})
}

func TestPytestRun_RealSuiteWhenAvailable(t *testing.T) {
	if _, err := exec.LookPath("pytest"); err != nil {
		t.Skip("pytest not installed on PATH")
	}
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/pytest")
		pytest := loadVariantRunner(t, variant, "pytest", RunnerConfig{})
		result := NewRunResult(nil)
		err := pytest.Run(result, []plan.TestCase{{Path: "test_sample.py"}}, false)
		require.NoError(t, err)
		assert.Equal(t, RunStatusPassed, result.Status())
		_ = os.Remove(pytest.ResultFilePath())
	})
}
