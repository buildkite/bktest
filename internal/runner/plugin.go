package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/buildkite/bktest/v3/internal/pluginhost"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/urfave/cli/v3"
)

// Plugin is a test runner implemented by an external runner plugin. bktest
// runs the commands the plugin builds and records the outcomes the plugin
// interprets; retries, muting and the job's status stay here.
type Plugin struct {
	RunnerConfig
	client *pluginhost.Client
	info   runnerplugin.InitializeResult
}

var (
	_ TestRunnerWithTargetDiscovery = (*Plugin)(nil)
	_ ExampleDiscoverer             = (*Plugin)(nil)
	_ io.Closer                     = (*Plugin)(nil)
)

// NewPlugin starts the plugin and initializes it with the runner config.
func NewPlugin(m pluginhost.Manifest, r RunnerConfig, debug bool) (*Plugin, error) {
	client, err := pluginhost.Start(m)
	if err != nil {
		return nil, err
	}
	params := runnerplugin.InitializeParams{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Config: runnerplugin.Config{
			TestCommand:            r.TestCommand,
			RetryTestCommand:       r.RetryTestCommand,
			TestFilePattern:        r.TestFilePattern,
			TestFileExcludePattern: r.TestFileExcludePattern,
			ResultPath:             r.ResultPath,
			LocationPrefix:         r.locationPrefix,
			TagFilters:             r.TagFilters,
			SelectorListPath:       r.SelectorListPath,
			Debug:                  debug,
		},
	}
	var info runnerplugin.InitializeResult
	if err := client.Call(runnerplugin.MethodInitialize, params, &info); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("runner plugin %q failed to initialize: %w", m.Runner, err)
	}
	if info.ProtocolVersion != runnerplugin.ProtocolVersion {
		_ = client.Close()
		return nil, fmt.Errorf("runner plugin %q uses protocol version %d; this bktest supports version %d", m.Runner, info.ProtocolVersion, runnerplugin.ProtocolVersion)
	}
	r.ResultPath = info.ResultPath
	r.locationPrefix = info.LocationPrefix
	return &Plugin{RunnerConfig: r, client: client, info: info}, nil
}

func (p *Plugin) Close() error { return p.client.Close() }

func (p *Plugin) Name() string { return p.info.Name }

func (p *Plugin) ResultFormat() string { return p.info.ResultFormat }

func (p *Plugin) SupportedFeatures() SupportedFeatures {
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

func (p *Plugin) DiscoverTestTargets() ([]string, error) {
	var result runnerplugin.DiscoverResult
	if err := p.client.Call(runnerplugin.MethodDiscover, nil, &result); err != nil {
		return nil, err
	}
	return result.Targets, nil
}

func (p *Plugin) GetExamples(files []string) ([]plan.TestCase, error) {
	var result runnerplugin.ExamplesResult
	if err := p.client.Call(runnerplugin.MethodExamples, runnerplugin.ExamplesParams{Files: files}, &result); err != nil {
		return nil, err
	}
	return fromPluginTestCases(result.TestCases), nil
}

func (p *Plugin) command(testCases []plan.TestCase, retry bool) (runnerplugin.CommandResult, error) {
	var result runnerplugin.CommandResult
	params := runnerplugin.CommandParams{TestCases: toPluginTestCases(testCases), Retry: retry}
	if err := p.client.Call(runnerplugin.MethodCommand, params, &result); err != nil {
		return result, err
	}
	if len(result.Argv) == 0 {
		return result, fmt.Errorf("runner plugin %q returned an empty command", p.Name())
	}
	return result, nil
}

func (p *Plugin) CommandNameAndArgs(testCases []plan.TestCase, retry bool) (string, []string, error) {
	result, err := p.command(testCases, retry)
	if err != nil {
		return "", nil, err
	}
	return result.Argv[0], result.Argv[1:], nil
}

// Run runs the plugin's command for the test cases, then records the outcomes
// the plugin interprets from it. Like the built-in runners, it returns the
// command's error, so a failed command still fails the job.
func (p *Plugin) Run(result *RunResult, testCases []plan.TestCase, retry bool) error {
	command, err := p.command(testCases, retry)
	if err != nil {
		return fmt.Errorf("failed to build command: %w", err)
	}
	cmd := commandWithEnvironment(p, command.Argv[0], command.Argv[1:])

	var cmdErr error
	if command.StdoutPath == "" {
		cmdErr = runAndForwardSignal(cmd)
	} else {
		file, err := os.Create(command.StdoutPath)
		if err != nil {
			return err
		}
		cmdErr = runAndForwardSignalWithOutput(cmd, io.MultiWriter(os.Stdout, file), os.Stderr)
		file.Close()
	}

	// A signaled command ends the run without its results.
	if signaled := new(ProcessSignaledError); errors.As(cmdErr, &signaled) {
		return cmdErr
	}

	params := runnerplugin.InterpretParams{TestCases: toPluginTestCases(testCases), Retry: retry}
	if exitErr := new(exec.ExitError); errors.As(cmdErr, &exitErr) {
		code := exitErr.ExitCode()
		params.ExitCode = &code
	} else if cmdErr == nil {
		code := 0
		params.ExitCode = &code
	}
	if cmdErr != nil {
		params.Error = cmdErr.Error()
	}

	var interpreted runnerplugin.InterpretResult
	if err := p.client.Call(runnerplugin.MethodInterpret, params, &interpreted); err != nil {
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
	if interpreted.Error != "" {
		result.error = errors.New(interpreted.Error)
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

func toPluginTestCases(testCases []plan.TestCase) []runnerplugin.TestCase {
	out := make([]runnerplugin.TestCase, len(testCases))
	for i, tc := range testCases {
		out[i] = runnerplugin.TestCase{
			EstimatedDuration: tc.EstimatedDuration,
			Format:            string(tc.Format),
			Identifier:        tc.Identifier,
			Name:              tc.Name,
			Path:              tc.Path,
			Scope:             tc.Scope,
			Value:             tc.Value,
		}
	}
	return out
}

func fromPluginTestCase(tc runnerplugin.TestCase) plan.TestCase {
	return plan.TestCase{
		EstimatedDuration: tc.EstimatedDuration,
		Format:            plan.TestCaseFormat(tc.Format),
		Identifier:        tc.Identifier,
		Name:              tc.Name,
		Path:              tc.Path,
		Scope:             tc.Scope,
		Value:             tc.Value,
	}
}

func fromPluginTestCases(testCases []runnerplugin.TestCase) []plan.TestCase {
	out := make([]plan.TestCase, len(testCases))
	for i, tc := range testCases {
		out[i] = fromPluginTestCase(tc)
	}
	return out
}
