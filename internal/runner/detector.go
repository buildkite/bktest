package runner

import (
	"fmt"
	"io"

	"github.com/buildkite/bktest/v3/internal/config"
	"github.com/buildkite/bktest/v3/internal/pluginhost"
)

// newExternalRunner loads the runner plugin at path, a plugin directory or
// its plugin.json, and configures it.
func newExternalRunner(path string, runnerConfig RunnerConfig) (TestRunnerWithTargetDiscovery, error) {
	manifest, err := pluginhost.LoadManifest(path)
	if err != nil {
		return nil, err
	}
	if manifest.Runner != runnerConfig.TestRunner {
		return nil, fmt.Errorf("runner plugin %q is for the test runner %q, not %q", path, manifest.Runner, runnerConfig.TestRunner)
	}
	plugin, err := NewPluginRunner(pluginhost.External{Manifest: manifest}, runnerConfig)
	if err != nil {
		return nil, fmt.Errorf("runner plugin %q: %w", path, err)
	}
	plugin.manifest = &manifest
	return plugin, nil
}

// Close releases what the runner holds for the bktest command, such as a
// runner plugin's state directory.
func Close(r TestRunner) {
	if closer, ok := r.(io.Closer); ok {
		_ = closer.Close()
	}
}

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

	// Only an explicit plugin is loaded from disk. A built-in runner is
	// selected by name alone, whatever files the working directory has.
	if cfg.RunnerPlugin != "" {
		return newExternalRunner(cfg.RunnerPlugin, runnerConfig)
	}

	switch testRunner := cfg.TestRunner; testRunner {
	case "rspec":
		return NewRspec(runnerConfig), nil
	case "jest":
		return NewJest(runnerConfig), nil
	case "vitest":
		return NewVitest(runnerConfig), nil
	case "cypress":
		return NewCypress(runnerConfig), nil
	case "playwright":
		return NewPlaywright(runnerConfig), nil
	case "pytest":
		return NewPytest(runnerConfig), nil
	case "gotest":
		return NewGoTest(runnerConfig), nil
	case "cucumber":
		return NewCucumber(runnerConfig), nil
	case "custom":
		return NewCustom(runnerConfig)
	default:
		// Update the error message to include the new runner
		return nil, fmt.Errorf("runner value %q is invalid, possible values are 'rspec', 'jest', 'vitest', 'cypress', 'playwright', 'pytest', 'gotest', 'cucumber', or 'custom'", testRunner)
	}
}
