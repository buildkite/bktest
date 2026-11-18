package command

import (
	"github.com/buildkite/bktest/v3/internal/config"
	"github.com/buildkite/bktest/v3/internal/runner"
)

func getTestTargets(cfg *config.Config, testRunner runner.TestRunnerWithTargetDiscovery, testFileList string) ([]string, error) {
	if shouldUseSelectorSplitting(testRunner) {
		if cfg.SelectorListPath != "" {
			return getRowsFromFile(cfg.SelectorListPath)
		}
	}

	if testFileList != "" {
		return getRowsFromFile(testFileList)
	}
	return testRunner.DiscoverTestTargets()
}
