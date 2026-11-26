package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
)

func TestCustom_NewCustom_MissingTestCommand(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		_, err := tryLoadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:      "",
			TestFilePattern:  "tests/**/test_*.sh",
			RetryTestCommand: "",
		})

		if err == nil || err.Error() != "test command must be provided for custom runner" {
			t.Errorf("load custom error = %v, want %q", err, "test command must be provided for custom runner")
		}
	})
}

func TestCustom_NewCustom_MissingTestFilePattern(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		_, err := tryLoadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:      "bin/test",
			TestFilePattern:  "",
			RetryTestCommand: "",
		})

		if err == nil || err.Error() != "test file pattern must be provided for custom runner" {
			t.Errorf("load custom error = %v, want %q", err, "test file pattern must be provided for custom runner")
		}
	})
}

func TestCustom_NewCustom_SelectorListWithoutTestFilePattern(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		_, err := tryLoadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:      "bin/test",
			TestFilePattern:  "",
			SelectorListPath: "selectors.txt",
		})

		if err != nil {
			t.Errorf("load custom error = %v, want nil", err)
		}
	})
}

func TestCustom_DiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/custom")
		custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:     "bats {{testExamples}}",
			TestFilePattern: "tests/*.bats",
		})

		got, err := custom.DiscoverTestTargets()
		if err != nil {
			t.Errorf("Custom.DiscoverTestTargets() error = %v", err)
		}

		want := []string{
			"tests/failed_test.bats",
			"tests/flaky_test.bats",
			"tests/happy_test.bats",
		}

		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("Custom.DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestCustom_CommandNameAndArgs(t *testing.T) {
	testCases := []plan.TestCase{{Path: "tests/test_a.sh"}, {Path: "tests/test_b.sh"}}

	commands := []struct {
		command  string
		wantName string
		wantArgs []string
	}{
		{
			command:  "bin/test -- {{testExamples}}",
			wantName: "bin/test",
			wantArgs: []string{"--", "tests/test_a.sh", "tests/test_b.sh"},
		},
		{
			command:  "rake test TEST_FILES='{{testExamples}}'",
			wantName: "rake",
			wantArgs: []string{"test", "TEST_FILES=tests/test_a.sh tests/test_b.sh"},
		},
	}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		for _, tc := range commands {
			custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
				TestCommand:     tc.command,
				TestFilePattern: "tests/**/test_*.sh",
			})

			gotName, gotArgs, err := custom.CommandNameAndArgs(testCases, false)
			if err != nil {
				t.Errorf("Custom.CommandNameAndArgs(%q) error = %v", tc.command, err)
			}

			if gotName != tc.wantName {
				t.Errorf("Custom.CommandNameAndArgs(%q) name = %v, want %v", tc.command, gotName, tc.wantName)
			}

			if diff := cmp.Diff(gotArgs, tc.wantArgs); diff != "" {
				t.Errorf("Custom.CommandNameAndArgs(%q) args diff (-got +want):\n%s", tc.command, diff)
			}
		}
	})
}

func TestCustom_Run(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/custom")
		custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:     "bats {{testExamples}}",
			TestFilePattern: "tests/**/*.bats",
		})

		testCases := []plan.TestCase{
			{Path: "./tests/happy_test.bats"},
		}

		result := NewRunResult([]plan.TestCase{})
		err := custom.Run(result, testCases, false)

		if err != nil {
			t.Errorf("Custom.Run(%v) error = %v", testCases, err)
		}
		if result.Status() != RunStatusUnknown {
			t.Errorf("Custom.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
		}
	})
}

func TestCustom_Run_TestFailedWithoutResult(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/custom")
		custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:     "bats {{testExamples}}",
			TestFilePattern: "tests/**/*.bats",
		})

		testCases := []plan.TestCase{
			{Path: "./tests/failed_test.bats"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := custom.Run(result, testCases, false)

		if result.Status() != RunStatusUnknown {
			t.Errorf("Custom.Run() RunResult.Status = %v, want %v", result.Status(), RunStatusUnknown)
		}

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
	})
}

func TestCustom_Run_TestFailedWithXMLResult(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/custom")
		tmpDir := t.TempDir()

		custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:     fmt.Sprintf("bats --report-formatter junit --output %s {{testExamples}}", tmpDir),
			TestFilePattern: "tests/**/*.bats",
			ResultPath:      filepath.Join(tmpDir, "report.xml"),
		})

		testCases := []plan.TestCase{
			{Path: "./tests/failed_test.bats"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := custom.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusFailed {
			t.Errorf("Custom.Run() RunResult.Status = %v, want %v", result.Status(), RunStatusFailed)
		}

		if len(result.tests) != 1 {
			t.Errorf("Custom.Run() len(RunResult.tests) = %d, want %d", len(result.tests), 1)
		}
	})
}

func TestCustom_Run_TestFailedWithJSONResult(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/custom")
		custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
			TestCommand:     "bats {{testExamples}}",
			TestFilePattern: "tests/**/*.bats",
			ResultPath:      "./test-result.json",
		})

		testCases := []plan.TestCase{
			{Path: "./tests/happy_test.bats"},
			{Path: "./tests/failed_test.bats"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := custom.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)

		if result.Status() != RunStatusFailed {
			t.Errorf("Custom.Run() RunResult.Status = %v, want %v", result.Status(), RunStatusFailed)
		}

		if len(result.tests) != 2 {
			t.Errorf("Custom.Run() len(RunResult.tests) = %d, want %d", len(result.tests), 2)
		}
	})
}

func TestCustom_ResultFormat(t *testing.T) {
	cases := []struct {
		resultPath string
		want       string
	}{
		{"", ""},
		{"results.json", "json"},
		{"results.xml", "junit"},
		{"./path/to/results.xml", "junit"},
	}
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		for _, tc := range cases {
			custom := loadVariantRunner(t, variant, "custom", RunnerConfig{
				TestCommand:     "echo",
				TestFilePattern: "*.sh",
				ResultPath:      tc.resultPath,
			})
			if got := custom.ResultFormat(); got != tc.want {
				t.Errorf("ResultFormat(%q) = %q, want %q", tc.resultPath, got, tc.want)
			}
		}
	})
}
