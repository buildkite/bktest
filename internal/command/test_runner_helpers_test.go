package command

import (
	"testing"

	"github.com/buildkite/bktest/v3/internal/config"
	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/buildkite/bktest/v3/internal/runner"
)

func mustDetectFromRunnerConfig(t *testing.T, name string, rc runner.RunnerConfig) runner.TestRunnerWithTargetDiscovery {
	t.Helper()
	return mustDetectRunner(t, name, config.Config{
		TestCommand:            rc.TestCommand,
		RetryCommand:           rc.RetryTestCommand,
		ResultPath:             rc.ResultPath,
		TestFilePattern:        rc.TestFilePattern,
		TestFileExcludePattern: rc.TestFileExcludePattern,
		TagFilters:             rc.TagFilters,
		SelectorListPath:       rc.SelectorListPath,
	})
}

func mustDetectRunner(t *testing.T, name string, cfg config.Config) runner.TestRunnerWithTargetDiscovery {
	t.Helper()
	cfg.TestRunner = name
	r, err := runner.DetectRunner(&cfg)
	if err != nil {
		t.Fatalf("DetectRunner(%q) error = %v", name, err)
	}
	return r
}

// noPrefixRunner wraps a TestRunner so LocationPrefix returns "" (matching the
// zero-value struct behavior previously used in command unit tests).
type noPrefixRunner struct {
	runner.TestRunnerWithTargetDiscovery
}

func (n noPrefixRunner) LocationPrefix() string { return "" }

func (n noPrefixRunner) GetExamples(files []string) ([]plan.TestCase, error) {
	return n.TestRunnerWithTargetDiscovery.(runner.ExampleDiscoverer).GetExamples(files)
}

func mustDetectRunnerNoPrefix(t *testing.T, name string, cfg config.Config) runner.TestRunnerWithTargetDiscovery {
	t.Helper()
	r := mustDetectRunner(t, name, cfg)
	if _, ok := r.(runner.ExampleDiscoverer); ok {
		return noPrefixRunner{TestRunnerWithTargetDiscovery: r}
	}
	return noPrefixBasicRunner{TestRunnerWithTargetDiscovery: r}
}

type noPrefixBasicRunner struct {
	runner.TestRunnerWithTargetDiscovery
}

func (n noPrefixBasicRunner) LocationPrefix() string { return "" }
