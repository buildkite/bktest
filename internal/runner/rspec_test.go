package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRspec(t *testing.T) {
	cases := []struct {
		input RunnerConfig
		want  RunnerConfig
	}{
		{
			input: RunnerConfig{},
			want: RunnerConfig{
				TestCommand:            "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}",
				TestFilePattern:        "spec/**/*_spec.rb",
				TestFileExcludePattern: "",
				RetryTestCommand:       "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}",
				locationPrefix:         "./",
			},
		},
		{
			input: RunnerConfig{
				TestCommand:            "bin/rspec --format json --out {{resultPath}}",
				TestFilePattern:        "spec/models/**/*_spec.rb",
				TestFileExcludePattern: "spec/features/**/*_spec.rb",
				RetryTestCommand:       "bin/rspec --only-failures",
				locationPrefix:         "apps/web/",
			},
			want: RunnerConfig{
				TestCommand:            "bin/rspec --format json --out {{resultPath}}",
				TestFilePattern:        "spec/models/**/*_spec.rb",
				TestFileExcludePattern: "spec/features/**/*_spec.rb",
				RetryTestCommand:       "bin/rspec --only-failures",
				locationPrefix:         "apps/web/",
			},
		},
	}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		for _, c := range cases {
			got := loadVariantRunner(t, variant, "rspec", c.input)
			if diff := cmp.Diff(resolvedRunnerConfig(t, got), c.want, cmp.AllowUnexported(RunnerConfig{})); diff != "" {
				t.Errorf("Rspec(%v) diff (-got +want):\n%s", c.input, diff)
			}
		}
	})
}

func TestRspecRun(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --format json --out {{resultPath}}",
			ResultPath:  filepath.Join(t.TempDir(), "rspec.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/spells/expelliarmus_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		if err != nil {
			t.Errorf("Rspec.Run(%v) error = %v", testCases, err)
		}
		if len(result.tests) != 2 {
			t.Errorf("Rspec.Run(%v) len(RunResult.tests) = %d, want 2", testCases, len(result.tests))
		}
		if result.Status() != RunStatusPassed {
			t.Errorf("Rspec.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
		}
	})
}

func TestRspecRun_RetryCommand(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand:      "rspec --invalid-option",
			RetryTestCommand: "rspec --format json --out {{resultPath}}",
			ResultPath:       filepath.Join(t.TempDir(), "rspec.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/spells/expelliarmus_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, true)

		if err != nil {
			t.Errorf("Rspec.Run(%v) error = %v", testCases, err)
		}
		if result.Status() != RunStatusPassed {
			t.Errorf("Rspec.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusPassed)
		}
	})
}

func TestRspecRun_RetryWithGeneratedDescription(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		dir := t.TempDir()
		spec := filepath.Join(dir, "generated_spec.rb")
		require.NoError(t, os.WriteFile(spec, []byte(`
RSpec.describe "generated description" do
  before do
    unless File.exist?(__FILE__ + ".retried")
      File.write(__FILE__ + ".retried", "")
      raise "transient setup failure"
    end
  end

  subject { 7 }
  it { is_expected.to eq(7) }
end
`), 0600))
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --format json --out {{resultPath}}",
			ResultPath:  filepath.Join(dir, "result.json"),
		})
		result := NewRunResult(nil)
		require.ErrorAs(t, rspec.Run(result, []plan.TestCase{{Path: spec}}, false), new(*exec.ExitError))
		failed := result.FailedTests()
		require.Len(t, failed, 1)
		require.NoError(t, rspec.Run(result, failed, true))
		require.Equal(t, RunStatusPassed, result.Status())
		require.Equal(t, RunStatistics{Total: 1, PassedOnRetry: 1}, result.Statistics())
	})
}

func TestRspecRun_TestFailedWithResultFile(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --format json --out {{resultPath}}",
			ResultPath:  filepath.Join(t.TempDir(), "rspec.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/failure_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		wantFailedTests := []plan.TestCase{
			{
				Name:       "fails",
				Scope:      "Failure",
				Identifier: "./testdata/rspec/spec/failure_spec.rb[1:1]",
				Path:       "./testdata/rspec/spec/failure_spec.rb[1:1]",
			},
		}

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		assert.Equal(t, RunStatusFailed, result.Status())
		if diff := cmp.Diff(result.FailedTests(), wantFailedTests); diff != "" {
			t.Errorf("FailedTests() diff (-got +want):\n%s", diff)
		}
	})
}

func TestRspecRun_TestFailedWithoutResultFile(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec",
		})
		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/failure_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		assert.Equal(t, RunStatusUnknown, result.Status())
		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
	})
}

func TestRspecRun_TestSkipped(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --format json --out {{resultPath}} --format progress",
			ResultPath:  filepath.Join(t.TempDir(), "rspec.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/skipped_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		assert.NoError(t, err)
		assert.Equal(t, RunStatusPassed, result.Status())
		assert.Equal(t, []plan.TestCase{{Name: "is skipped", Scope: "skipped", Identifier: "./testdata/rspec/spec/skipped_spec.rb[1:1]", Path: "./testdata/rspec/spec/skipped_spec.rb[1:1]"}}, result.SkippedTests())
	})
}

func TestRspecRun_TestExit(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --format json --out {{resultPath}} --format progress",
			ResultPath:  filepath.Join(t.TempDir(), "rspec.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/exit_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		assert.Equal(t, RunStatusUnknown, result.Status())
	})
}

func TestRspecRun_ErrorOutsideOfExamples(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --format json --out {{resultPath}} --format documentation",
			ResultPath:  filepath.Join(t.TempDir(), "rspec.json"),
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/bad_syntax_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		assert.Equal(t, RunStatusError, result.Status())
	})
}

func TestRspecRun_CommandFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec --invalid-option",
		})
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, nil, false)

		assert.Equal(t, RunStatusUnknown, result.Status())
		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
	})
}

func TestRspecRun_SignaledError(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "./testdata/segv.sh",
		})
		testCases := []plan.TestCase{
			{Path: "./testdata/rspec/spec/failure_spec.rb"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := rspec.Run(result, testCases, false)

		assert.Equal(t, RunStatusUnknown, result.Status())
		signalError := new(ProcessSignaledError)
		require.ErrorAs(t, err, &signalError)
		assert.Equal(t, syscall.SIGSEGV, signalError.Signal)
	})
}

func TestRspecCommandNameAndArgs(t *testing.T) {
	testCases := []plan.TestCase{{Path: "spec/models/user_spec.rb"}, {Path: "spec/models/billing_spec.rb"}}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "bin/rspec --options {{testExamples}} --out {{resultPath}}",
			ResultPath:  "tmp/rspec.json",
		})
		gotName, gotArgs, err := rspec.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "bin/rspec", gotName)
		assert.Equal(t, []string{"--options", "spec/models/user_spec.rb", "spec/models/billing_spec.rb", "--out", "tmp/rspec.json"}, gotArgs)

		rspec2 := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "bin/rspec --options --format",
		})
		gotName, gotArgs, err = rspec2.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "bin/rspec", gotName)
		assert.Equal(t, []string{"--options", "--format", "spec/models/user_spec.rb", "spec/models/billing_spec.rb"}, gotArgs)

		rspec3 := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "bin/rspec --options ' {{testExamples}}",
		})
		_, _, err = rspec3.CommandNameAndArgs(testCases, false)
		if !errors.Is(err, shellquote.UnterminatedSingleQuoteError) {
			t.Errorf("error = %v, want %v", err, shellquote.UnterminatedSingleQuoteError)
		}
	})
}

func TestRspecDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantRunner(t, variant, "rspec", RunnerConfig{
			TestFilePattern: "testdata/rspec/spec/spells/**/*_spec.rb",
		})

		got, err := rspec.DiscoverTestTargets()
		require.NoError(t, err)

		want := []string{"testdata/rspec/spec/spells/expelliarmus_spec.rb"}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestRspecGetExamples(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantExampleRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec",
		})
		files := []string{"./testdata/rspec/spec/spells/expelliarmus_spec.rb"}
		got, err := rspec.GetExamples(files)
		require.NoError(t, err)

		want := []plan.TestCase{
			{
				Identifier: "./testdata/rspec/spec/spells/expelliarmus_spec.rb[1:1]",
				Name:       "disarms the opponent",
				Path:       "./testdata/rspec/spec/spells/expelliarmus_spec.rb[1:1]",
				Scope:      "Expelliarmus",
			},
			{
				Identifier: "./testdata/rspec/spec/spells/expelliarmus_spec.rb[1:2]",
				Name:       "knocks the wand out of the opponents hand",
				Path:       "./testdata/rspec/spec/spells/expelliarmus_spec.rb[1:2]",
				Scope:      "Expelliarmus",
			},
		}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("GetExamples() diff (-got +want):\n%s", diff)
		}
	})
}

func TestRspecGetExamples_WithSharedExamples(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		rspec := loadVariantExampleRunner(t, variant, "rspec", RunnerConfig{
			TestCommand: "rspec",
		})
		files := []string{"./testdata/rspec/spec/specs_with_shared_examples_spec.rb"}
		got, err := rspec.GetExamples(files)
		require.NoError(t, err)

		want := []plan.TestCase{
			{
				Identifier: "./testdata/rspec/spec/specs_with_shared_examples_spec.rb[1:1:1]",
				Name:       "behaves like a shared example",
				Path:       "./testdata/rspec/spec/specs_with_shared_examples_spec.rb[1:1:1]",
				Scope:      "Specs with shared examples behaves like shared",
			},
		}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("GetExamples() diff (-got +want):\n%s", diff)
		}
	})
}

func TestParseNativeRSpecReport(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		got, err := ParseNativeReportWithOptions("rspec-json", []byte(`{"examples":[
			{"id":"./spec/owning_spec.rb[1:1]","file_path":"./spec/shared.rb","line_number":12,"description":"works","full_description":"Shared works","status":"pending"},
			{"file_path":"./spec/fallback_spec.rb","line_number":7,"description":"fails","full_description":"Fallback fails","status":"failed"}
		],"summary":{"example_count":2,"errors_outside_of_examples_count":1}}`), nil, variant)
		if err != nil {
			t.Fatal(err)
		}
		want := ParsedReport{ErrorsOutsideTests: true, Tests: []ReportedTest{
			{TestCase: plan.TestCase{Identifier: "spec/owning_spec.rb[1:1]", Path: "spec/owning_spec.rb[1:1]", Name: "works", Scope: "Shared"}, Status: TestStatusSkipped, Selector: "spec/owning_spec.rb", Location: "spec/shared.rb:12"},
			{TestCase: plan.TestCase{Path: "spec/fallback_spec.rb:7", Name: "fails", Scope: "Fallback"}, Status: TestStatusFailed, Selector: "spec/fallback_spec.rb", Location: "spec/fallback_spec.rb:7"},
		}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("parsed report (-want +got):\n%s", diff)
		}
		if _, err := ParseNativeReportWithOptions("not-rspec-json", []byte(`{"examples":[],"summary":{"example_count":0}}`), nil, variant); err == nil {
			t.Fatal("unsupported report format accepted")
		}
	})
}
