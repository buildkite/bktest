package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewJest(t *testing.T) {
	cases := []struct {
		input RunnerConfig
		want  RunnerConfig
	}{
		{
			input: RunnerConfig{},
			want: RunnerConfig{
				TestCommand:            "npx jest --runTestsByPath {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}",
				TestFilePattern:        "**/{__tests__/**/*,*.spec,*.test}.{ts,js,tsx,jsx}",
				TestFileExcludePattern: "",
				RetryTestCommand:       "npx jest --testNamePattern '{{testNamePattern}}' --json --testLocationInResults --outputFile {{resultPath}}",
			},
		},
		{
			input: RunnerConfig{
				TestCommand:            "yarn test --json --outputFile {{resultPath}}",
				TestFilePattern:        "spec/models/**/*.spec.js",
				TestFileExcludePattern: "spec/features/**/*.spec.js",
				RetryTestCommand:       "yarn test --testNamePattern '{{testNamePattern}}' --json --testLocationInResults --outputFile {{resultPath}}",
			},
			want: RunnerConfig{
				TestCommand:            "yarn test --json --outputFile {{resultPath}}",
				TestFilePattern:        "spec/models/**/*.spec.js",
				TestFileExcludePattern: "spec/features/**/*.spec.js",
				RetryTestCommand:       "yarn test --testNamePattern '{{testNamePattern}}' --json --testLocationInResults --outputFile {{resultPath}}",
			},
		},
	}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		for _, c := range cases {
			got := loadVariantRunner(t, variant, "jest", c.input)
			if diff := cmp.Diff(resolvedRunnerConfig(t, got), c.want, cmp.AllowUnexported(RunnerConfig{})); diff != "" {
				t.Errorf("Jest(%v) diff (-got +want):\n%s", c.input, diff)
			}
		}
	})
}

func TestJestRun(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/jest")

		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "npx jest --json --outputFile {{resultPath}}",
			ResultPath:  filepath.Join(t.TempDir(), "jest.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/jest/spells/expelliarmus.spec.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := jest.Run(result, testCases, false)

		assert.NoError(t, err)
		assert.Len(t, result.tests, 1)
		assert.Equal(t, RunStatusPassed, result.Status())
	})
}

func TestJestRun_Retry(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/jest")

		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand:      "jest --invalid-option --json --outputFile {{resultPath}}",
			RetryTestCommand: "npx jest --testNamePattern '{{testNamePattern}}' --json --outputFile {{resultPath}} ./testdata/jest/spells/expelliarmus.spec.js ./testdata/jest/failure.spec.js",
			ResultPath:       filepath.Join(t.TempDir(), "jest.json"),
		})

		testCases := []plan.TestCase{
			{Scope: "expelliarmus", Name: "disarms the opponent"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := jest.Run(result, testCases, true)

		assert.NoError(t, err)
		assert.Equal(t, RunStatusPassed, result.Status())
	})
}

func TestJestRun_TestFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/jest")

		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "npx jest --json --outputFile {{resultPath}}",
			ResultPath:  filepath.Join(t.TempDir(), "jest.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/jest/failure.spec.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := jest.Run(result, testCases, false)

		wantFailedTests := []plan.TestCase{
			{Scope: "this will fail", Name: "for sure", Path: "failure.spec.js"},
		}
		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		assert.Equal(t, RunStatusFailed, result.Status())
		if diff := cmp.Diff(result.FailedTests(), wantFailedTests); diff != "" {
			t.Errorf("FailedTests() diff (-got +want):\n%s", diff)
		}
	})
}

func TestJestRun_TestSkipped(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/jest")

		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "npx jest --json --outputFile {{resultPath}}",
			ResultPath:  filepath.Join(t.TempDir(), "jest.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/jest/skipped.spec.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := jest.Run(result, testCases, false)

		assert.NoError(t, err)
		assert.Equal(t, RunStatusPassed, result.Status())
		assert.Equal(t, TestStatusSkipped, result.tests["this will be skipped/for sure/skipped.spec.js"].Status)
		assert.Equal(t, TestStatusSkipped, result.tests["this will be skipped/todo yeah/skipped.spec.js"].Status)
	})
}

func TestJestRun_RuntimeError(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/jest")

		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "npx jest --json --outputFile {{resultPath}}",
			ResultPath:  filepath.Join(t.TempDir(), "jest.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/jest/spells/expelliarmus.spec.js"},
			{Path: "./testdata/jest/runtimeError.spec.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := jest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		assert.Len(t, result.tests, 1)
		assert.Equal(t, RunStatusError, result.Status())
		assert.Equal(t, []string{"runtimeError.spec.js"}, result.ErroredFiles())
		assert.True(t, result.Retryable())
	})
}

func TestJestRun_SignaledError(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "./testdata/segv.sh --outputFile {{resultPath}}",
		})

		testCases := []plan.TestCase{{Path: "./doesnt-matter.spec.js"}}
		result := NewRunResult([]plan.TestCase{})
		err := jest.Run(result, testCases, false)

		assert.Equal(t, RunStatusUnknown, result.Status())
		signalError := new(ProcessSignaledError)
		require.ErrorAs(t, err, &signalError)
		assert.Equal(t, syscall.SIGSEGV, signalError.Signal)
	})
}

func TestJestCommandNameAndArgs(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		testCases := []plan.TestCase{{Path: "spec/user.spec.js"}, {Path: "spec/billing.spec.js"}}
		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "jest {{testExamples}} --outputFile {{resultPath}}",
			ResultPath:  "jest.json",
		})
		gotName, gotArgs, err := jest.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "jest", gotName)
		assert.Equal(t, []string{"spec/billing.spec.js", "spec/user.spec.js", "--outputFile", "jest.json"}, gotArgs)

		jestInvalid := loadVariantRunner(t, variant, "jest", RunnerConfig{
			TestCommand: "jest --options '{{testExamples}}",
		})
		_, _, err = jestInvalid.CommandNameAndArgs(testCases, false)
		if !errors.Is(err, shellquote.UnterminatedSingleQuoteError) {
			t.Errorf("error = %v, want %v", err, shellquote.UnterminatedSingleQuoteError)
		}

		// Retry with special regex characters
		retryCases := []plan.TestCase{
			{Scope: "test", Name: "with special characters .+*?()|[]{}^$", Path: "spec/user.spec.js"},
			{Scope: "another", Name: "test", Path: "spec/billing.spec.js"},
			{Name: "top-level test", Path: "spec/billing.spec.js"},
		}
		jestRetry := loadVariantRunner(t, variant, "jest", RunnerConfig{
			RetryTestCommand: "jest --testNamePattern '{{testNamePattern}}' {{testExamples}} --json --testLocationInResults --outputFile {{resultPath}}",
			ResultPath:       "jest.json",
		})
		gotName, gotArgs, err = jestRetry.CommandNameAndArgs(retryCases, true)
		require.NoError(t, err)
		assert.Equal(t, "jest", gotName)
		assert.Equal(t, []string{
			"--testNamePattern",
			`^(test with special characters \.\+\*\?\(\)\|\[\]\{\}\^\$|another test|top-level test)$`,
			"spec/billing.spec.js",
			"spec/user.spec.js",
			"--json",
			"--testLocationInResults",
			"--outputFile",
			"jest.json",
		}, gotArgs)

		// Missing sentinel
		jestMissing := loadVariantRunner(t, variant, "jest", RunnerConfig{
			RetryTestCommand: "jest --json --outputFile {{resultPath}}",
			ResultPath:       "jest.json",
		})
		_, _, err = jestMissing.CommandNameAndArgs(retryCases, true)
		require.EqualError(t, err, "couldn't find '{{testNamePattern}}' sentinel in retry command")
	})
}

func TestJestDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/jest")
		jest := loadVariantRunner(t, variant, "jest", RunnerConfig{})

		got, err := jest.DiscoverTestTargets()
		require.NoError(t, err)

		want := []string{
			"failure.spec.js",
			"runtimeError.spec.js",
			"skipped.spec.js",
			"slow.spec.js",
			"spells/expelliarmus.spec.js",
		}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestParseNativeJestReport(t *testing.T) {
	workDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		got, err := ParseNativeReportWithOptions("jest-json", []byte(`{"testResults":[
			{"name":"`+workDir+`/src/a.test.js","status":"failed","assertionResults":[
				{"title":"adds","ancestorTitles":["math","sum"],"status":"failed","location":{"line":3,"column":5}},
				{"title":"each","ancestorTitles":[],"status":"failed"},
				{"title":"each","ancestorTitles":[],"status":"passed"},
				{"title":"later","ancestorTitles":[],"status":"todo"}
			]},
			{"name":"b.test.js","status":"passed","assertionResults":[]}
		]}`), nil, variant)
		if err != nil {
			t.Fatal(err)
		}
		want := ParsedReport{Tests: []ReportedTest{
			{TestCase: plan.TestCase{Path: "src/a.test.js", Scope: "math sum", Name: "adds"}, Status: TestStatusFailed, Selector: "src/a.test.js", Location: "src/a.test.js:3"},
			{TestCase: plan.TestCase{Path: "src/a.test.js", Name: "each"}, Status: TestStatusFailed, Selector: "src/a.test.js"},
			{TestCase: plan.TestCase{Path: "src/a.test.js", Name: "later"}, Status: TestStatusSkipped, Selector: "src/a.test.js"},
		}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("parsed report (-want +got):\n%s", diff)
		}

		got, err = ParseNativeReportWithOptions("jest-json", []byte(`{"numRuntimeErrorTestSuites":2,"testResults":[
			{"name":"c.test.js","status":"failed","assertionResults":[{"title":"t","ancestorTitles":[],"status":"passed"}]},
			{"name":"d.test.js","status":"failed","assertionResults":[]},
			{"name":"e.test.js","status":"passed","assertionResults":[{"title":"t","ancestorTitles":[],"status":"passed"}]}
		]}`), nil, variant)
		if err != nil || got.ErrorsOutsideTests || !reflect.DeepEqual(got.ErroredFiles, []string{"c.test.js", "d.test.js"}) {
			t.Fatalf("report=%+v err=%v, want only c.test.js and d.test.js errored", got, err)
		}

		got, err = ParseNativeReportWithOptions("jest-json", []byte(`{"numRuntimeErrorTestSuites":1,"testResults":[
			{"name":"f.test.js","status":"failed","assertionResults":[{"title":"t","ancestorTitles":[],"status":"failed"}]},
			{"name":"g.test.js","status":"failed","assertionResults":[{"title":"t","ancestorTitles":[],"status":"failed"}]}
		]}`), nil, variant)
		if err != nil || !got.ErrorsOutsideTests {
			t.Fatalf("report=%+v err=%v, want errors outside tests", got, err)
		}

		for _, invalid := range []string{`{}`, `{"testResults":[{"name":"c.test.js","assertionResults":[{"title":"t","status":"focused"}]}]}`} {
			if _, err := ParseNativeReportWithOptions("jest-json", []byte(invalid), nil, variant); err == nil {
				t.Errorf("accepted invalid report %s", invalid)
			}
		}
	})
}
