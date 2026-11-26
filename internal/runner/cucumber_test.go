package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCucumber(t *testing.T) {
	cases := []struct {
		input RunnerConfig
		want  RunnerConfig
	}{
		{
			input: RunnerConfig{},
			want: RunnerConfig{
				TestCommand:            "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}",
				TestFilePattern:        "features/**/*.feature",
				TestFileExcludePattern: "",
				RetryTestCommand:       "cucumber --format pretty --format json --out {{resultPath}} {{testExamples}}",
			},
		},
		{
			input: RunnerConfig{
				TestCommand:            "bundle exec cucumber --format json --out {{resultPath}} {{testExamples}}",
				TestFilePattern:        "custom_features/**/*.feature",
				TestFileExcludePattern: "custom_features/wip/**/*.feature",
				RetryTestCommand:       "bundle exec cucumber --retry",
			},
			want: RunnerConfig{
				TestCommand:            "bundle exec cucumber --format json --out {{resultPath}} {{testExamples}}",
				TestFilePattern:        "custom_features/**/*.feature",
				TestFileExcludePattern: "custom_features/wip/**/*.feature",
				RetryTestCommand:       "bundle exec cucumber --retry",
			},
		},
	}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		for _, c := range cases {
			got := loadVariantRunner(t, variant, "cucumber", c.input)
			if diff := cmp.Diff(resolvedRunnerConfig(t, got), c.want, cmp.AllowUnexported(RunnerConfig{})); diff != "" {
				t.Errorf("Cucumber(%v) diff (-got +want):\n%s", c.input, diff)
			}
		}
	})
}

func TestCucumberDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/cucumber")
		cucumber := loadVariantRunner(t, variant, "cucumber", RunnerConfig{})

		got, err := cucumber.DiscoverTestTargets()
		require.NoError(t, err)

		want := []string{
			"features/another_feature.feature",
			"features/failure.feature",
			"features/simple_scenarios.feature",
			"features/spells/expelliarmus.feature",
		}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestCucumberCommandNameAndArgs(t *testing.T) {
	testCases := []plan.TestCase{{Path: "features/spells/expelliarmus.feature"}, {Path: "features/failure.feature"}}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		c := loadVariantRunner(t, variant, "cucumber", RunnerConfig{
			TestCommand: "cucumber --format json --out {{resultPath}} {{testExamples}}",
			ResultPath:  "cucumber.json",
		})
		gotName, gotArgs, err := c.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "cucumber", gotName)
		assert.Equal(t, []string{"--format", "json", "--out", "cucumber.json", "features/spells/expelliarmus.feature", "features/failure.feature"}, gotArgs)

		c2 := loadVariantRunner(t, variant, "cucumber", RunnerConfig{
			TestCommand: "cucumber --format json --out {{resultPath}}",
			ResultPath:  "cucumber.json",
		})
		gotName, gotArgs, err = c2.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "cucumber", gotName)
		assert.Equal(t, []string{"--format", "json", "--out", "cucumber.json", "features/spells/expelliarmus.feature", "features/failure.feature"}, gotArgs)

		c3 := loadVariantRunner(t, variant, "cucumber", RunnerConfig{
			TestCommand: "cucumber --format json --out '{{resultPath}} {{testExamples}}",
		})
		_, _, err = c3.CommandNameAndArgs(testCases, false)
		if !errors.Is(err, shellquote.UnterminatedSingleQuoteError) {
			t.Errorf("error = %v, want %v", err, shellquote.UnterminatedSingleQuoteError)
		}
	})
}

func TestCucumberRun_StepAggregationFromReport(t *testing.T) {
	reportJSON := `[
		{
			"uri": "features/simple_scenarios.feature",
			"name": "Simple Scenarios",
			"elements": [
				{"type": "background", "name": "BG", "line": 3, "steps": [{"result": {"status": "passed"}}]},
				{"type": "scenario", "name": "Passing", "line": 5, "steps": [{"result": {"status": "passed"}}]},
				{"type": "scenario", "name": "Pending", "line": 15, "steps": [{"result": {"status": "passed"}}, {"result": {"status": "pending"}}]},
				{"type": "scenario", "name": "Skipped", "line": 19, "steps": [{"result": {"status": "skipped"}}]},
				{"type": "scenario", "name": "Failing", "line": 23, "steps": [{"result": {"status": "passed"}}, {"result": {"status": "failed"}}, {"result": {"status": "skipped"}}]}
			]
		}
	]`

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		reportPath := filepath.Join(t.TempDir(), "cucumber.json")
		require.NoError(t, os.WriteFile(reportPath, []byte(reportJSON), 0o600))

		c := loadVariantRunner(t, variant, "cucumber", RunnerConfig{
			TestCommand: "true --out {{resultPath}}",
			ResultPath:  reportPath,
		})
		res := NewRunResult(nil)
		require.NoError(t, c.Run(res, []plan.TestCase{{Path: "features/simple_scenarios.feature"}}, false))

		assert.Equal(t, RunStatusFailed, res.Status())
		stats := res.Statistics()
		assert.Equal(t, 4, stats.Total)
		assert.Equal(t, 1, stats.PassedOnFirstRun)
		assert.Equal(t, 2, stats.Skipped)
		assert.Equal(t, 1, stats.Failed)
		assert.Equal(t, []plan.TestCase{{
			Identifier: "features/simple_scenarios.feature:23",
			Name:       "Failing",
			Scope:      "Simple Scenarios",
			Path:       "features/simple_scenarios.feature:23",
		}}, res.FailedTests())
	})
}

func TestCucumberGetExamples_EmptyFiles(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		c := loadVariantExampleRunner(t, variant, "cucumber", RunnerConfig{})
		got, err := c.GetExamples([]string{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}
