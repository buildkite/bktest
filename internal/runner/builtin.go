package runner

import (
	"fmt"
	"os"

	"github.com/buildkite/bktest/v3/internal/pluginhost"
	"github.com/buildkite/bktest/v3/internal/runners"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

// The built-in runners are runner plugins that bktest calls in its own
// process. They are selected by name alone, and need no files on disk.

// newBuiltin configures the built-in runner with the given name.
func newBuiltin(name string, r RunnerConfig) (*PluginRunner, error) {
	return NewPluginRunner(pluginhost.InProcess{New: runners.Builtin[name]}, r)
}

// mustBuiltin is for the runners that accept every configuration.
func mustBuiltin(name string, r RunnerConfig) *PluginRunner {
	runner, err := newBuiltin(name, r)
	if err != nil {
		panic(fmt.Sprintf("configuring the %s runner: %v", name, err))
	}
	return runner
}

func NewRspec(r RunnerConfig) *PluginRunner      { return mustBuiltin("rspec", r) }
func NewJest(r RunnerConfig) *PluginRunner       { return mustBuiltin("jest", r) }
func NewVitest(r RunnerConfig) *PluginRunner     { return mustBuiltin("vitest", r) }
func NewCypress(r RunnerConfig) *PluginRunner    { return mustBuiltin("cypress", r) }
func NewPlaywright(r RunnerConfig) *PluginRunner { return mustBuiltin("playwright", r) }
func NewGoTest(r RunnerConfig) *PluginRunner     { return mustBuiltin("gotest", r) }
func NewCucumber(r RunnerConfig) *PluginRunner   { return mustBuiltin("cucumber", r) }

// NewPytest exits when pytest can't be run as configured, such as when a
// setting needs a buildkite-test-collector that isn't installed.
func NewPytest(r RunnerConfig) *PluginRunner {
	runner, err := newBuiltin("pytest", r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	return runner
}

func NewCustom(r RunnerConfig) (*PluginRunner, error) { return newBuiltin("custom", r) }

// batchReportRunners are the built-in runners that parse each report format
// of Test Scheduler pools.
var batchReportRunners = map[string]string{
	"rspec-json":      "rspec",
	"jest-json":       "jest",
	"playwright-json": "playwright",
	"pytest-json":     "pytest",
	"pytest-junit":    "pytest",
}

// ParseNativeReport parses a report in a format of one of the built-in
// runners, such as a persistent runner's report. A completed protocol
// envelope only means the runner produced a report.
func ParseNativeReport(format string, data []byte) (ParsedReport, error) {
	name, ok := batchReportRunners[format]
	if !ok {
		return ParsedReport{}, fmt.Errorf("unsupported runner report format %q", format)
	}
	return parseBatchReport(pluginhost.InProcess{New: runners.Builtin[name]}, runnerplugin.Config{}, os.TempDir(), format, data)
}
