package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
)

func TestGotestRun(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			ResultPath: filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		assert.NoError(t, err)
		if result.Status() != RunStatusPassed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
		}

		testResult := result.tests["example.com/hello/TestHelloWorld/example.com/hello"]
		if testResult == nil || testResult.Path != "example.com/hello" {
			t.Errorf("TestResult = %v, want Path example.com/hello", testResult)
		}
	})
}

func TestGotestRun_TestFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			ResultPath: filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello/bad"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusFailed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusFailed)
		}
	})
}

func TestGotestRun_BuildFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			ResultPath: filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello/broken"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusError {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusError)
		}
		if failed := result.FailedTests(); len(failed) != 0 {
			t.Errorf("Gotest.Run(%v) RunResult.FailedTests() = %v, want none", testCases, failed)
		}
	})
}

func TestGotestRun_GoJSONL(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "go test -json {{packages}}",
			ResultPath:  filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		assert.NoError(t, err)
		if result.Status() != RunStatusPassed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
		}

		testResult := result.tests["example.com/hello/TestHelloWorld/example.com/hello"]
		if testResult == nil || testResult.Path != "example.com/hello" {
			t.Errorf("TestResult = %v, want Path example.com/hello", testResult)
		}
	})
}

func TestGotestRun_GotestsumGoJSONL(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "gotestsum --jsonfile={{resultPath}} {{packages}}",
			ResultPath:  filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		assert.NoError(t, err)
		if result.Status() != RunStatusPassed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
		}

		testResult := result.tests["example.com/hello/TestHelloWorld/example.com/hello"]
		if testResult == nil || testResult.Path != "example.com/hello" {
			t.Errorf("TestResult = %v, want Path example.com/hello", testResult)
		}
	})
}

func TestGotestRun_GoJSONLTestFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "go test -json {{packages}}",
			ResultPath:  filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello/bad"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusFailed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusFailed)
		}

		failed := result.FailedTests()
		if len(failed) != 1 || failed[0].Path != "example.com/hello/bad" {
			t.Errorf("Gotest.Run(%v) RunResult.FailedTests = %v, want one failed package", testCases, failed)
		}
	})
}

func TestGotestRun_GoJSONLBuildFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "go test -json {{packages}}",
			ResultPath:  filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello/broken"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusError {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusError)
		}
		if failed := result.FailedTests(); len(failed) != 0 {
			t.Errorf("Gotest.Run(%v) RunResult.FailedTests() = %v, want none", testCases, failed)
		}
	})
}

func TestGotestRun_GoJSONLPackageLevelFailure(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "go test -json {{packages}}",
			ResultPath:  filepath.Join(t.TempDir(), "test-results.jsonl"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello/testmain"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		if result.Status() != RunStatusFailed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusFailed)
		}

		failed := result.FailedTests()
		if len(failed) != 1 || failed[0].Path != "example.com/hello/testmain" || failed[0].Name != "TestMain" {
			t.Errorf("Gotest.Run(%v) RunResult.FailedTests = %v, want package-level failure", testCases, failed)
		}
	})
}

func TestGotestRun_GoJSONLFailureIsNotOverwrittenByLaterPass(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		resultPath := filepath.Join(t.TempDir(), "test-results.jsonl")
		err := os.WriteFile(resultPath, []byte(`{"Action":"start","Package":"example.com/hello"}
{"Action":"fail","Package":"example.com/hello","Test":"TestFlaky"}
{"Action":"pass","Package":"example.com/hello","Test":"TestFlaky"}
{"Action":"fail","Package":"example.com/hello"}
`), 0o600)
		assert.NoError(t, err)

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "true --jsonfile={{resultPath}} {{packages}}",
			ResultPath:  resultPath,
		})
		result := NewRunResult([]plan.TestCase{})
		err = gotest.Run(result, []plan.TestCase{{Path: "example.com/hello"}}, false)

		assert.NoError(t, err)
		if result.Status() != RunStatusFailed {
			t.Errorf("GoTest RunResult.Status = %v, want %v", result.Status(), RunStatusFailed)
		}

		failed := result.FailedTests()
		if len(failed) != 1 || failed[0].Name != "TestFlaky" {
			t.Errorf("GoTest RunResult.FailedTests = %v, want TestFlaky failure", failed)
		}
	})
}

func TestGotestRun_CommandFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "gotestsum --junitfile {{resultPath}} bluhbluh",
			ResultPath:  filepath.Join(t.TempDir(), "test-results.xml"),
		})
		testCases := []plan.TestCase{
			{Path: "example.com/hello"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := gotest.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusFailed {
			t.Errorf("Gotest.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusFailed)
		}
	})
}

func TestGotestUploadResultFormats(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		cases := []struct {
			name       string
			cfg        RunnerConfig
			wantFormat string
			wantPath   string
		}{
			{
				name:       "defaults to go-jsonl",
				cfg:        RunnerConfig{ResultPath: "go-test.jsonl"},
				wantFormat: "go-jsonl",
				wantPath:   "go-test.jsonl",
			},
			{
				name:       "go test -json",
				cfg:        RunnerConfig{ResultPath: "go-test.jsonl", TestCommand: "go test -json {{packages}}"},
				wantFormat: "go-jsonl",
				wantPath:   "go-test.jsonl",
			},
			{
				name:       "gotestsum --jsonfile",
				cfg:        RunnerConfig{ResultPath: "go-test.jsonl", TestCommand: "gotestsum --jsonfile={{resultPath}} {{packages}}"},
				wantFormat: "go-jsonl",
				wantPath:   "go-test.jsonl",
			},
			{
				name:       "gotestsum --junitfile with -- -json",
				cfg:        RunnerConfig{ResultPath: "junit.xml", TestCommand: "gotestsum --junitfile={{resultPath}} -- -json {{packages}}"},
				wantFormat: "junit",
				wantPath:   "junit.xml",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				gotest := loadVariantRunner(t, variant, "gotest", tc.cfg)
				assert.Equal(t, tc.wantFormat, gotest.ResultFormat())
				assert.Equal(t, tc.wantPath, gotest.ResultFilePath())
			})
		}
	})
}

func TestGotestCommands(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			ResultPath:  "go-test.jsonl",
			TestCommand: "gotestsum --jsonfile={{resultPath}} -- -count=1 {{packages}}",
		})
		cmd, args, err := gotest.CommandNameAndArgs([]plan.TestCase{{Path: "example.com/hello"}}, false)
		assert.NoError(t, err)
		assert.Equal(t, "gotestsum", cmd)
		assert.Equal(t, []string{"--jsonfile=go-test.jsonl", "--", "-count=1", "example.com/hello"}, args)

		gotest2 := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			ResultPath:  "go-test.jsonl",
			TestCommand: "go test -json -count=1 {{packages}}",
		})
		cmd, args, err = gotest2.CommandNameAndArgs([]plan.TestCase{{Path: "example.com/hello"}}, false)
		assert.NoError(t, err)
		assert.Equal(t, "go", cmd)
		assert.Equal(t, []string{"test", "-json", "-count=1", "example.com/hello"}, args)

		gotest3 := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			ResultPath:  "junit.xml",
			TestCommand: "gotestsum --junitfile={{resultPath}} -- -count=1 {{packages}}",
		})
		cmd, args, err = gotest3.CommandNameAndArgs([]plan.TestCase{{Path: "example.com/hello"}}, false)
		assert.NoError(t, err)
		assert.Equal(t, "gotestsum", cmd)
		assert.Equal(t, []string{"--junitfile=junit.xml", "--", "-count=1", "example.com/hello"}, args)
	})
}

func TestGotestDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/go")

		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{})

		got, err := gotest.DiscoverTestTargets()
		if err != nil {
			t.Errorf("Gotest.DiscoverTestTargets() error = %v", err)
		}

		want := []string{
			"example.com/hello",
			"example.com/hello/bad",
			"example.com/hello/broken",
			"example.com/hello/testmain",
		}

		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("Gotest.DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestGotestCommandNameAndArgs_SelectorAndPathTasks(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		gotest := loadVariantRunner(t, variant, "gotest", RunnerConfig{
			TestCommand: "gotestsum --junitfile={{resultPath}} {{packages}}",
			ResultPath:  "test-results.xml",
		})

		testCases := []plan.TestCase{
			{Format: plan.TestCaseFormatSelector, Value: "example.com/hello"},
			{Format: plan.TestCaseFormatSelector, Value: "example.com/hello/bad"},
			{Format: plan.TestCaseFormatSelector, Value: "example.com/hello"},
		}

		gotName, gotArgs, err := gotest.CommandNameAndArgs(testCases, false)
		if err != nil {
			t.Fatalf("GoTest.CommandNameAndArgs() error = %v", err)
		}
		assert.Equal(t, "gotestsum", gotName)
		assert.Equal(t, []string{"--junitfile=test-results.xml", "example.com/hello", "example.com/hello/bad"}, gotArgs)
	})
}
