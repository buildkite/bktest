package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

	"github.com/buildkite/bktest/v3/internal/debug"
	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/buildkite/bktest/v3/internal/pluginhost"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/urfave/cli/v3"
)

// PluginRunner is a test runner implemented by a runner plugin: a built-in
// handler called in this process, or an external plugin. bktest runs the
// commands the plugin prepares and records the outcomes the plugin
// interprets; retries, muting and the job's status stay in bktest.
type PluginRunner struct {
	// RunnerConfig is the configuration with the plugin's defaults applied.
	RunnerConfig
	plugin   pluginhost.Plugin
	info     runnerplugin.ConfigureResponse
	stateDir string
	// manifest is set for an external plugin.
	manifest *pluginhost.Manifest
}

var (
	_ TestRunnerWithTargetDiscovery = (*PluginRunner)(nil)
	_ ExampleDiscoverer             = (*PluginRunner)(nil)
	_ io.Closer                     = (*PluginRunner)(nil)
)

// NewPluginRunner configures the plugin with the runner config. An error
// means the plugin rejected the configuration or couldn't be called.
func NewPluginRunner(plugin pluginhost.Plugin, r RunnerConfig) (*PluginRunner, error) {
	// The plugin keeps what it needs between operations here. Close removes it.
	stateDir, err := os.MkdirTemp("", "bktest-runner-*")
	if err != nil {
		return nil, err
	}
	p := &PluginRunner{RunnerConfig: r, plugin: plugin, stateDir: stateDir}
	p.info, err = plugin.Configure(runnerplugin.ConfigureRequest{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Config:          p.pluginConfig(),
		StateDir:        stateDir,
	})
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	resolved := p.info.Config
	p.TestCommand = resolved.TestCommand
	p.RetryTestCommand = resolved.RetryTestCommand
	p.TestFilePattern = resolved.TestFilePattern
	p.TestFileExcludePattern = resolved.TestFileExcludePattern
	p.ResultPath = resolved.ResultPath
	p.locationPrefix = resolved.LocationPrefix
	p.TagFilters = resolved.TagFilters
	p.SelectorListPath = resolved.SelectorListPath
	return p, nil
}

func (p *PluginRunner) pluginConfig() runnerplugin.Config {
	return runnerplugin.Config{
		TestCommand:            p.TestCommand,
		RetryTestCommand:       p.RetryTestCommand,
		TestFilePattern:        p.TestFilePattern,
		TestFileExcludePattern: p.TestFileExcludePattern,
		ResultPath:             p.ResultPath,
		LocationPrefix:         p.locationPrefix,
		TagFilters:             p.TagFilters,
		SelectorListPath:       p.SelectorListPath,
		PersistentRunner:       p.persistentRunner,
		Debug:                  debug.Enabled,
	}
}

func (p *PluginRunner) supports(operation string) bool {
	return slices.Contains(p.info.Operations, operation)
}

// Close removes the plugin's state directory.
func (p *PluginRunner) Close() error { return os.RemoveAll(p.stateDir) }

func (p *PluginRunner) Name() string { return p.info.Name }

// Source describes where an external plugin was loaded from, for bktest's
// output. It is empty for a built-in runner.
func (p *PluginRunner) Source() string {
	if p.manifest == nil {
		return ""
	}
	return p.manifest.String()
}

func (p *PluginRunner) ResultFormat() string { return p.info.ResultFormat }

// BatchReportFormat is the format of the report at the result path, for
// ParseBatchReport.
func (p *PluginRunner) BatchReportFormat() string {
	if p.info.BatchReportFormat != "" {
		return p.info.BatchReportFormat
	}
	return p.info.ResultFormat
}

func (p *PluginRunner) SupportedFeatures() SupportedFeatures {
	f := p.info.Features
	return SupportedFeatures{
		SplitByFile:     f.SplitByFile,
		SplitByExample:  f.SplitByExample,
		SplitBySelector: f.SplitBySelector,
		FilterTestFiles: f.FilterTestFiles,
		FilterTestByTag: f.FilterTestByTag,
		AutoRetry:       f.AutoRetry,
		Mute:            f.Mute,
		Skip:            f.Skip,
	}
}

// DiscoverTestTargets returns the plugin's targets. Unless the plugin finds
// its own, they are the files that match its file patterns, found with
// bktest's matcher so every plugin globs the same way.
func (p *PluginRunner) DiscoverTestTargets() ([]string, error) {
	pattern, excludePattern := p.TestFilePattern, p.TestFileExcludePattern
	if p.supports(runnerplugin.OpDiscoverTargets) {
		response, err := p.plugin.DiscoverTargets(runnerplugin.DiscoverTargetsRequest{
			ProtocolVersion: runnerplugin.ProtocolVersion,
			Config:          p.pluginConfig(),
			StateDir:        p.stateDir,
		})
		if err != nil {
			return nil, err
		}
		if response.Pattern == "" {
			return response.Targets, nil
		}
		pattern, excludePattern = response.Pattern, response.ExcludePattern
	}

	debug.Println("Discovering test files with include pattern:", pattern, "exclude pattern:", excludePattern)
	files, err := discoverTestFiles(pattern, excludePattern)
	debug.Println("Discovered", len(files), "files")
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found with pattern %q and exclude pattern %q", pattern, excludePattern)
	}
	return files, nil
}

func (p *PluginRunner) GetExamples(files []string) ([]plan.TestCase, error) {
	if !p.supports(runnerplugin.OpDiscoverExamples) {
		return nil, fmt.Errorf("runner %q does not implement example discovery", p.Name())
	}
	response, err := p.plugin.DiscoverExamples(runnerplugin.DiscoverExamplesRequest{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Config:          p.pluginConfig(),
		StateDir:        p.stateDir,
		Files:           files,
	})
	if err != nil {
		return nil, err
	}
	return fromPluginTestCases(response.TestCases), nil
}

func (p *PluginRunner) prepareAttempt(testCases []plan.TestCase, retry bool) (runnerplugin.PrepareAttemptResponse, error) {
	response, err := p.plugin.PrepareAttempt(runnerplugin.PrepareAttemptRequest{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Config:          p.pluginConfig(),
		StateDir:        p.stateDir,
		TestCases:       toPluginTestCases(testCases),
		Retry:           retry,
	})
	if err != nil {
		return response, err
	}
	if len(response.Argv) == 0 {
		return response, fmt.Errorf("runner %q prepared an empty command", p.Name())
	}
	return response, nil
}

func (p *PluginRunner) CommandNameAndArgs(testCases []plan.TestCase, retry bool) (string, []string, error) {
	response, err := p.prepareAttempt(testCases, retry)
	if err != nil {
		return "", []string{}, err
	}
	return response.Argv[0], response.Argv[1:], nil
}

// Run runs the command the plugin prepares for the test cases, then records
// the outcomes the plugin interprets from it. It returns the command's
// error, so a failed command fails the job whatever the plugin reports.
func (p *PluginRunner) Run(result *RunResult, testCases []plan.TestCase, retry bool) error {
	prepared, err := p.prepareAttempt(testCases, retry)
	if err != nil {
		return fmt.Errorf("failed to build command: %w", err)
	}
	cmd := commandWithEnvironment(p, prepared.Argv[0], prepared.Argv[1:])

	var cmdErr error
	if prepared.StdoutPath == "" {
		cmdErr = runAndForwardSignal(cmd)
	} else {
		file, err := os.Create(prepared.StdoutPath)
		if err != nil {
			// The command didn't run. The plugin still interprets the
			// attempt, as it does for a command that couldn't start.
			cmdErr = err
		} else {
			cmdErr = runAndForwardSignalWithOutput(cmd, io.MultiWriter(os.Stdout, file), os.Stderr)
			file.Close()
		}
	}

	if !p.supports(runnerplugin.OpInterpretAttempt) {
		return cmdErr
	}

	request := runnerplugin.InterpretAttemptRequest{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Config:          p.pluginConfig(),
		StateDir:        p.stateDir,
		TestCases:       toPluginTestCases(testCases),
		Retry:           retry,
	}
	if exitErr := new(exec.ExitError); errors.As(cmdErr, &exitErr) {
		code := exitErr.ExitCode()
		request.ExitCode = &code
	} else if cmdErr == nil {
		code := 0
		request.ExitCode = &code
	}
	if cmdErr != nil {
		request.Error = cmdErr.Error()
	}
	interpreted, err := p.plugin.InterpretAttempt(request)
	if err != nil {
		return err
	}

	for _, message := range interpreted.Messages {
		fmt.Println(message)
	}
	for _, test := range interpreted.Tests {
		testCase := fromPluginTestCase(test.TestCase)
		key := test.Key
		if key == "" {
			key = testIdentifier(testCase)
		}
		result.recordTestResultWithKey(key, testCase, TestStatus(test.Status))
	}
	switch {
	case interpreted.RunError == "":
	case cmdErr != nil && interpreted.RunError == cmdErr.Error():
		// The plugin reported the command's error as the run's error. Keep
		// the error itself, with its exit code.
		result.error = cmdErr
	default:
		result.error = errors.New(interpreted.RunError)
	}
	for _, file := range interpreted.ErroredFiles {
		result.recordErroredFile(file)
	}

	if len(interpreted.Rerun) > 0 {
		// The command failed for the tests that caused the rerun, even if
		// the rerun passes.
		if err := p.Run(result, fromPluginTestCases(interpreted.Rerun), retry); err != nil {
			return err
		}
		return cmdErr
	}

	if interpreted.FailedTestsMessage != "" && cmdErr == nil && len(result.FailedTests()) > 0 {
		return cli.Exit(interpreted.FailedTestsMessage, 1)
	}
	return cmdErr
}

// ParseBatchReport parses a report for Test Scheduler pools.
func (p *PluginRunner) ParseBatchReport(format string, data []byte) (ParsedReport, error) {
	if !p.supports(runnerplugin.OpParseBatchReport) {
		return ParsedReport{}, fmt.Errorf("unsupported runner report format %q", format)
	}
	// Plugins read the report from a file, like the reports they interpret
	// after a test command. A file also suits a script better than a large
	// JSON string.
	file, err := os.CreateTemp(p.stateDir, "batch-report-*")
	if err != nil {
		return ParsedReport{}, err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return ParsedReport{}, err
	}
	response, err := p.plugin.ParseBatchReport(runnerplugin.ParseBatchReportRequest{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Config:          p.pluginConfig(),
		StateDir:        p.stateDir,
		Format:          format,
		ReportPath:      file.Name(),
	})
	if err != nil {
		return ParsedReport{}, err
	}
	parsed := ParsedReport{
		ErrorsOutsideTests:     response.ErrorsOutsideTests,
		ErroredFiles:           response.ErroredFiles,
		ErroredFilesStoppedRun: response.ErroredFilesStoppedRun,
		UnselectedTests:        response.UnselectedTests,
	}
	for _, test := range response.Tests {
		parsed.Tests = append(parsed.Tests, ReportedTest{
			TestCase: fromPluginTestCase(test.TestCase),
			Status:   TestStatus(test.Status),
			Selector: test.Selector,
			Location: test.Location,
		})
	}
	return parsed, nil
}

func toPluginTestCases(testCases []plan.TestCase) []runnerplugin.TestCase {
	out := make([]runnerplugin.TestCase, len(testCases))
	for i, tc := range testCases {
		out[i] = runnerplugin.TestCase{
			Format:     string(tc.Format),
			Identifier: tc.Identifier,
			Name:       tc.Name,
			Path:       tc.Path,
			Scope:      tc.Scope,
			Value:      tc.Value,
		}
	}
	return out
}

func fromPluginTestCase(tc runnerplugin.TestCase) plan.TestCase {
	return plan.TestCase{
		Format:     plan.TestCaseFormat(tc.Format),
		Identifier: tc.Identifier,
		Name:       tc.Name,
		Path:       tc.Path,
		Scope:      tc.Scope,
		Value:      tc.Value,
	}
}

func fromPluginTestCases(testCases []runnerplugin.TestCase) []plan.TestCase {
	var out []plan.TestCase
	for _, tc := range testCases {
		out = append(out, fromPluginTestCase(tc))
	}
	return out
}
