package command

import (
	"fmt"
	"os"

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

// announceRunnerPlugin says which external runner plugin runs the tests. It
// writes to stderr, because plan commands print their result to stdout.
func announceRunnerPlugin(testRunner runner.TestRunner) {
	if plugin, ok := testRunner.(*runner.PluginRunner); ok && plugin.Source() != "" {
		fmt.Fprintf(os.Stderr, "Buildkite Test Engine Client: using the %s runner plugin at %s\n", plugin.Name(), plugin.Source())
	}
}
