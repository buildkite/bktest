package main

import (
	"fmt"
	"os"

	"github.com/buildkite/bktest/v3/internal/config"
	"github.com/buildkite/bktest/v3/internal/runner"
)

func printTableRow(cols ...string) {
	for _, col := range cols {
		fmt.Printf("| %s ", col)
	}
	fmt.Printf("|\n")
}

func boolToEmoji(b bool) string {
	if b {
		return "✅"
	} else {
		return "❌"
	}
}

func printRow(runners []runner.TestRunner, featureName string, featureValueFunc func(runner.TestRunner) bool) {
	var values []string
	values = append(values, featureName)
	for _, runner := range runners {
		values = append(values, boolToEmoji(featureValueFunc(runner)))
	}
	printTableRow(values...)
}

func main() {
	runnerNames := []string{
		"rspec",
		"jest",
		"vitest",
		"playwright",
		"cypress",
		"pytest",
		"gotest",
		"cucumber",
		"custom",
	}

	runners := make([]runner.TestRunner, 0, len(runnerNames))
	for _, name := range runnerNames {
		cfg := config.New()
		cfg.TestRunner = name
		cfg.TestCommand = "foo"
		cfg.TestFilePattern = "**/*_test.*"
		r, err := runner.DetectRunner(&cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load runner plugin %q: %v\n", name, err)
			os.Exit(1)
		}
		runners = append(runners, r)
	}

	headings := []string{"Feature"}
	for _, runner := range runners {
		headings = append(headings, runner.Name())
	}
	printTableRow(headings...)

	separators := []string{"---"}
	for range runners {
		separators = append(separators, ":---:")
	}
	printTableRow(separators...)

	printRow(
		runners,
		"[Selector-based test splitting](https://github.com/buildkite/test-engine-client/blob/main/README.md#selector-based-test-splitting)",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().SplitBySelector },
	)

	printRow(
		runners,
		"[Split slow files by individual test example](https://github.com/buildkite/test-engine-client/blob/main/docs/rspec.md#split-slow-files-by-individual-test-example)",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().SplitByExample },
	)

	printRow(
		runners,
		"Filter test files",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().FilterTestFiles },
	)

	printRow(
		runners,
		"Filter tests by tag",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().FilterTestByTag },
	)

	printRow(
		runners,
		"Automatically retry failed test",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().AutoRetry },
	)

	printRow(
		runners,
		"Mute tests (ignore test failures)",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().Mute },
	)

	printRow(
		runners,
		"Skip tests",
		func(r runner.TestRunner) bool { return r.SupportedFeatures().Skip },
	)
}
