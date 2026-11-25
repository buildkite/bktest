package runner

import (
	"slices"
	"testing"
)

func TestSupportedFeatures_SplitBySelectorSupportedRunners(t *testing.T) {
	runnerConfig := RunnerConfig{
		ResultPath:       "results.json",
		TestCommand:      "test-command",
		TestFilePattern:  "**/*_test.go",
		RetryTestCommand: "retry-command",
	}

	allNames := []string{"custom", "rspec", "jest", "playwright", "cypress", "pytest", "gotest", "cucumber", "vitest"}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		var runners []TestRunner
		var supportedRunners []string
		for _, name := range allNames {
			r := loadVariantRunner(t, variant, name, runnerConfig)
			runners = append(runners, r)
			supportedRunners = append(supportedRunners, r.Name())
		}

		for _, r := range runners {
			got := r.SupportedFeatures().SplitBySelector
			want := slices.Contains(supportedRunners, r.Name())
			if got != want {
				t.Errorf("%s (%s) SplitBySelector = %v, want %v", r.Name(), variant, got, want)
			}
		}
	})
}

func TestSupportedFeatures_SplitByExampleRequiresExampleDiscoverer(t *testing.T) {
	runnerConfig := RunnerConfig{
		ResultPath:      "results.json",
		TestCommand:     "test-command",
		TestFilePattern: "**/*_test.go",
	}

	allNames := []string{"custom", "rspec", "jest", "playwright", "cypress", "pytest", "gotest", "cucumber", "vitest"}

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		for _, name := range allNames {
			testRunner := loadVariantRunner(t, variant, name, runnerConfig)
			_, implementsExampleDiscovery := testRunner.(ExampleDiscoverer)
			if got := testRunner.SupportedFeatures().SplitByExample; got != implementsExampleDiscovery {
				t.Errorf("%s (%s) SplitByExample = %v, implements ExampleDiscoverer = %v", testRunner.Name(), variant, got, implementsExampleDiscovery)
			}
		}
	})
}
