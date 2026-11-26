package runner

import (
	"github.com/buildkite/bktest/v3/internal/config"
	hostplugin "github.com/buildkite/bktest/v3/internal/plugin"
)

// DetectRunner resolves and configures a runner plugin from cfg.
// All built-in runners (rspec, jest, vitest, cypress, playwright, pytest,
// gotest, cucumber, custom) are implemented as external plugins.
func DetectRunner(cfg *config.Config) (TestRunnerWithTargetDiscovery, error) {
	runnerConfig := RunnerConfig{
		TestRunner: cfg.TestRunner,

		locationPrefix:         cfg.LocationPrefix,
		ResultPath:             cfg.ResultPath,
		RetryTestCommand:       cfg.RetryCommand,
		TagFilters:             cfg.TagFilters,
		TestCommand:            cfg.TestCommand,
		TestFileExcludePattern: cfg.TestFileExcludePattern,
		TestFilePattern:        cfg.TestFilePattern,
		uploadToken:            cfg.UploadToken,
		testProcessEnv:         cfg.TestProcessEnv,
		SelectorListPath:       cfg.SelectorListPath,
		persistentRunner:       cfg.PersistentRunner,
	}

	loaded, err := hostplugin.Resolve(hostplugin.ResolveOptions{
		PluginPath:    cfg.PluginPath,
		TestRunner:    cfg.TestRunner,
		PluginDirs:    cfg.PluginDirs,
		PluginVariant: cfg.PluginVariant,
	})
	if err != nil {
		return nil, err
	}

	return NewPluginRunner(loaded, runnerConfig)
}

// DetectRunnerFromPlugin loads a specific plugin path (directory, subdirectory,
// "root//subdir", or executable) and configures it with rc.
func DetectRunnerFromPlugin(pluginRef string, rc RunnerConfig) (TestRunnerWithTargetDiscovery, error) {
	loaded, err := hostplugin.Load(pluginRef)
	if err != nil {
		return nil, err
	}
	return NewPluginRunner(loaded, rc)
}
