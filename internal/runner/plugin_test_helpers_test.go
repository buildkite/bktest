package runner

import (
	"path/filepath"
	"runtime"
	"testing"

	hostplugin "github.com/buildkite/bktest/v3/internal/plugin"
)

var allPluginVariants = []string{"go", "bash"}

func repoPluginsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "plugins"))
}

func pluginSubdirPath(t *testing.T, variant, name string) string {
	t.Helper()
	return filepath.Join(repoPluginsDir(t), variant, name)
}

func forEachPluginVariant(t *testing.T, fn func(t *testing.T, variant string)) {
	t.Helper()
	for _, variant := range allPluginVariants {
		t.Run(variant, func(t *testing.T) {
			fn(t, variant)
		})
	}
}

func loadVariantRunner(t *testing.T, variant, name string, cfg RunnerConfig) TestRunnerWithTargetDiscovery {
	t.Helper()
	r, err := tryLoadVariantRunner(t, variant, name, cfg)
	if err != nil {
		t.Fatalf("load plugin %s/%s: %v", variant, name, err)
	}
	return r
}

func tryLoadVariantRunner(t *testing.T, variant, name string, cfg RunnerConfig) (TestRunnerWithTargetDiscovery, error) {
	t.Helper()
	path := pluginSubdirPath(t, variant, name)
	loaded, err := hostplugin.Load(path)
	if err != nil {
		return nil, err
	}
	return NewPluginRunner(loaded, cfg)
}

func loadVariantExampleRunner(t *testing.T, variant, name string, cfg RunnerConfig) ExampleDiscoverer {
	t.Helper()
	r := loadVariantRunner(t, variant, name, cfg)
	ed, ok := r.(ExampleDiscoverer)
	if !ok {
		t.Fatalf("plugin %s/%s does not implement ExampleDiscoverer", variant, name)
	}
	return ed
}

func resolvedRunnerConfig(t *testing.T, r TestRunner) RunnerConfig {
	t.Helper()
	switch v := r.(type) {
	case *PluginRunner:
		return v.ResolvedConfig()
	case *PluginRunnerWithExamples:
		return v.ResolvedConfig()
	default:
		t.Fatalf("unexpected runner type %T", r)
		return RunnerConfig{}
	}
}
