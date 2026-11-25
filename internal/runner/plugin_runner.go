package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/buildkite/bktest/v3/internal/plan"
	hostplugin "github.com/buildkite/bktest/v3/internal/plugin"
	proto "github.com/buildkite/bktest/v3/pkg/plugin"
	"github.com/kballard/go-shellquote"
	"github.com/urfave/cli/v3"
)

// PluginRunner adapts an external bktest runner plugin (Go binary or Bash
// script) to the internal TestRunnerWithTargetDiscovery interface.
type PluginRunner struct {
	RunnerConfig
	loaded                    *hostplugin.LoadedPlugin
	runnerID                  string
	displayName               string
	resultFormat              string
	batchReportFormat         string
	requiresResultPath        bool
	poolNonRetryableExitCodes bool
	features                  SupportedFeatures
	capabilities              proto.Capabilities

	mu    sync.Mutex
	state json.RawMessage
}

// PluginRunnerWithExamples wraps PluginRunner to implement ExampleDiscoverer
// only when the plugin advertises example discovery support.
type PluginRunnerWithExamples struct {
	*PluginRunner
}

var (
	_ TestRunnerWithTargetDiscovery = (*PluginRunner)(nil)
	_ TestRunnerWithTargetDiscovery = (*PluginRunnerWithExamples)(nil)
	_ ExampleDiscoverer             = (*PluginRunnerWithExamples)(nil)
)

// NewPluginRunner configures a loaded plugin with cfg and returns a
// TestRunnerWithTargetDiscovery (implementing ExampleDiscoverer when the
// plugin supports example discovery).
func NewPluginRunner(loaded *hostplugin.LoadedPlugin, cfg RunnerConfig) (TestRunnerWithTargetDiscovery, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determine working directory: %w", err)
	}

	req := proto.ConfigureRequest{
		ProtocolVersion: proto.ProtocolVersion,
		WorkingDir:      wd,
		Config:          toWireSettings(cfg),
	}

	var resp proto.ConfigureResponse
	if err := loaded.Invoke(proto.OpConfigure, req, &resp); err != nil {
		return nil, err
	}

	for _, warning := range resp.Warnings {
		fmt.Fprintln(os.Stderr, warning)
	}

	resolved := fromWireSettings(resp.Config, cfg)
	runnerID := resp.RunnerID
	if runnerID == "" {
		runnerID = loaded.Manifest.Name
	}
	if runnerID == "" {
		runnerID = cfg.TestRunner
	}
	displayName := resp.Name
	if displayName == "" {
		displayName = loaded.Manifest.DisplayName
	}
	if displayName == "" {
		displayName = runnerID
	}

	pr := &PluginRunner{
		RunnerConfig:              resolved,
		loaded:                    loaded,
		runnerID:                  runnerID,
		displayName:               displayName,
		resultFormat:              resp.ResultFormat,
		batchReportFormat:         resp.BatchReportFormat,
		requiresResultPath:        resp.RequiresResultPath,
		poolNonRetryableExitCodes: resp.PoolNonRetryableExitCodes,
		features:                  fromWireFeatures(resp.Features),
		capabilities:              resp.Capabilities,
		state:                     resp.State,
	}

	if pr.capabilities.DiscoverExamples || pr.features.SplitByExample {
		return &PluginRunnerWithExamples{PluginRunner: pr}, nil
	}
	return pr, nil
}

func (p *PluginRunner) Name() string {
	return p.displayName
}

// RunnerID returns the canonical runner identifier (e.g. "rspec", "pytest")
// sent to Test Engine APIs even when loaded via an explicit plugin path.
func (p *PluginRunner) RunnerID() string {
	return p.runnerID
}

func (p *PluginRunner) SupportedFeatures() SupportedFeatures {
	return p.features
}

func (p *PluginRunner) ResultFormat() string {
	return p.resultFormat
}

func (p *PluginRunner) BatchReportFormat() string {
	if p.batchReportFormat != "" {
		return p.batchReportFormat
	}
	return p.resultFormat
}

func (p *PluginRunner) RequiresResultPath() bool {
	return p.requiresResultPath
}

func (p *PluginRunner) PoolNonRetryableExitCodes() bool {
	return p.poolNonRetryableExitCodes
}

func (p *PluginRunner) Plugin() *hostplugin.LoadedPlugin {
	return p.loaded
}

func (p *PluginRunner) ResolvedConfig() RunnerConfig {
	return p.RunnerConfig
}

func (p *PluginRunner) currentState() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.state) == 0 {
		return nil
	}
	cp := make(json.RawMessage, len(p.state))
	copy(cp, p.state)
	return cp
}

func (p *PluginRunner) setState(state json.RawMessage) {
	if len(state) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make(json.RawMessage, len(state))
	copy(cp, state)
	p.state = cp
}

func (p *PluginRunner) DiscoverTestTargets() ([]string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determine working directory: %w", err)
	}

	req := proto.DiscoverTargetsRequest{
		ProtocolVersion: proto.ProtocolVersion,
		WorkingDir:      wd,
		Config:          toWireSettings(p.RunnerConfig),
		State:           p.currentState(),
	}

	var resp proto.DiscoverTargetsResponse
	if err := p.loaded.Invoke(proto.OpDiscoverTargets, req, &resp); err != nil {
		return nil, err
	}
	if resp.Targets == nil {
		return []string{}, nil
	}
	return resp.Targets, nil
}

func (p *PluginRunnerWithExamples) GetExamples(files []string) ([]plan.TestCase, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determine working directory: %w", err)
	}

	req := proto.DiscoverExamplesRequest{
		ProtocolVersion: proto.ProtocolVersion,
		WorkingDir:      wd,
		Config:          toWireSettings(p.RunnerConfig),
		Files:           files,
		State:           p.currentState(),
	}

	var resp proto.DiscoverExamplesResponse
	if err := p.loaded.Invoke(proto.OpDiscoverExamples, req, &resp); err != nil {
		return nil, err
	}
	return fromWireTestCases(resp.Examples), nil
}

func (p *PluginRunner) prepareAttempt(testCases []plan.TestCase, retry bool) (proto.PrepareAttemptResponse, error) {
	wd, err := os.Getwd()
	if err != nil {
		return proto.PrepareAttemptResponse{}, fmt.Errorf("determine working directory: %w", err)
	}

	req := proto.PrepareAttemptRequest{
		ProtocolVersion: proto.ProtocolVersion,
		WorkingDir:      wd,
		Config:          toWireSettings(p.RunnerConfig),
		TestCases:       toWireTestCases(testCases),
		Retry:           retry,
		State:           p.currentState(),
	}

	var resp proto.PrepareAttemptResponse
	if err := p.loaded.Invoke(proto.OpPrepareAttempt, req, &resp); err != nil {
		return proto.PrepareAttemptResponse{}, mapShellquoteError(err)
	}
	if resp.State != nil {
		p.setState(resp.State)
	}
	if resp.Args == nil {
		resp.Args = []string{}
	}
	return resp, nil
}

func (p *PluginRunner) CommandNameAndArgs(testCases []plan.TestCase, retry bool) (string, []string, error) {
	resp, err := p.prepareAttempt(testCases, retry)
	if err != nil {
		return "", []string{}, err
	}
	return resp.Command, resp.Args, nil
}

func (p *PluginRunner) Run(result *RunResult, testCases []plan.TestCase, retry bool) error {
	prep, err := p.prepareAttempt(testCases, retry)
	if err != nil {
		return err
	}

	cmd := exec.Command(prep.Command, prep.Args...)
	overrides := map[string]string{
		"BUILDKITE_ANALYTICS_TOKEN": p.UploadToken(),
	}
	for k, v := range p.testProcessEnvironment() {
		overrides[k] = v
	}
	for k, v := range prep.Env {
		overrides[k] = v
	}
	cmd.Env = environmentWithOverrides(os.Environ(), overrides)

	var cmdErr error
	if prep.StdoutTeePath != "" {
		file, createErr := os.Create(prep.StdoutTeePath)
		if createErr != nil {
			return createErr
		}
		cmdErr = runAndForwardSignalWithOutput(cmd, io.MultiWriter(os.Stdout, file), os.Stderr)
		_ = file.Close()
	} else {
		cmdErr = runAndForwardSignal(cmd)
	}

	if !p.capabilities.InterpretAttempt {
		return cmdErr
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine working directory: %w", err)
	}

	interpretReq := proto.InterpretAttemptRequest{
		ProtocolVersion: proto.ProtocolVersion,
		WorkingDir:      wd,
		Config:          toWireSettings(p.RunnerConfig),
		TestCases:       toWireTestCases(testCases),
		Retry:           retry,
		CommandFailed:   cmdErr != nil,
		State:           p.currentState(),
	}
	if cmdErr != nil {
		interpretReq.CommandError = cmdErr.Error()
		var exitErr *exec.ExitError
		if errors.As(cmdErr, &exitErr) {
			interpretReq.ExitCode = exitErr.ExitCode()
		}
		var sigErr *ProcessSignaledError
		if errors.As(cmdErr, &sigErr) {
			interpretReq.Signaled = true
			interpretReq.Signal = int(sigErr.Signal)
		}
	}

	var resp proto.InterpretAttemptResponse
	if err := p.loaded.Invoke(proto.OpInterpretAttempt, interpretReq, &resp); err != nil {
		return err
	}

	if resp.State != nil {
		p.setState(resp.State)
	}

	if resp.ParseWarning != "" {
		fmt.Println(resp.ParseWarning)
	}

	for _, outcome := range resp.Results {
		tc := fromWireTestCase(outcome.TestCase)
		status := TestStatus(outcome.Status)
		if outcome.Key != "" {
			result.recordTestResultWithKey(outcome.Key, tc, status)
		} else {
			result.RecordTestResult(tc, status)
		}
	}

	for _, file := range resp.ErroredFiles {
		result.recordErroredFile(file)
	}

	if resp.RunError != "" {
		result.error = errors.New(resp.RunError)
	} else if resp.UseCommandErrorAsRunError && result.Status() != RunStatusError && cmdErr != nil {
		result.error = cmdErr
	}

	if len(resp.RerunTestCases) > 0 {
		if resp.RerunMessage != "" {
			fmt.Println(resp.RerunMessage)
		}
		if rerunErr := p.Run(result, fromWireTestCases(resp.RerunTestCases), retry); rerunErr != nil {
			return rerunErr
		}
		return cmdErr
	}

	if resp.FailIfFailedTests && cmdErr == nil && len(result.FailedTests()) > 0 {
		msg := resp.FailIfFailedTestsMessage
		if msg == "" {
			msg = fmt.Sprintf("%s finished with failed tests; see the report above", p.displayName)
		}
		return cli.Exit(msg, 1)
	}

	return cmdErr
}

// ParseBatchReportWithPlugin invokes parse-batch-report on a loaded plugin.
func ParseBatchReportWithPlugin(loaded *hostplugin.LoadedPlugin, format string, data []byte) (ParsedReport, error) {
	wd, err := os.Getwd()
	if err != nil {
		return ParsedReport{}, fmt.Errorf("determine working directory: %w", err)
	}

	req := proto.ParseBatchReportRequest{
		ProtocolVersion: proto.ProtocolVersion,
		WorkingDir:      wd,
		Format:          format,
		ReportBase64:    base64.StdEncoding.EncodeToString(data),
	}

	var resp proto.ParseBatchReportResponse
	if err := loaded.Invoke(proto.OpParseBatchReport, req, &resp); err != nil {
		return ParsedReport{}, err
	}

	parsed := ParsedReport{
		ErrorsOutsideTests:     resp.ErrorsOutsideTests,
		ErroredFilesStoppedRun: resp.ErroredFilesStoppedRun,
		UnselectedTests:        resp.UnselectedTests,
	}
	if len(resp.ErroredFiles) > 0 {
		parsed.ErroredFiles = resp.ErroredFiles
	}
	if len(resp.Tests) > 0 {
		parsed.Tests = make([]ReportedTest, len(resp.Tests))
		for i, t := range resp.Tests {
			parsed.Tests[i] = ReportedTest{
				TestCase: fromWireTestCase(t.TestCase),
				Status:   TestStatus(t.Status),
				Selector: t.Selector,
				Location: t.Location,
			}
		}
	}
	return parsed, nil
}

func mapShellquoteError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, shellquote.UnterminatedSingleQuoteError.Error()):
		return shellquote.UnterminatedSingleQuoteError
	case strings.Contains(msg, shellquote.UnterminatedDoubleQuoteError.Error()):
		return shellquote.UnterminatedDoubleQuoteError
	case strings.Contains(msg, shellquote.UnterminatedEscapeError.Error()):
		return shellquote.UnterminatedEscapeError
	default:
		return err
	}
}

func toWireSettings(c RunnerConfig) proto.RunnerSettings {
	return proto.RunnerSettings{
		TestRunner:             c.TestRunner,
		LocationPrefix:         c.locationPrefix,
		ResultPath:             c.ResultPath,
		RetryTestCommand:       c.RetryTestCommand,
		TagFilters:             c.TagFilters,
		TestCommand:            c.TestCommand,
		TestFileExcludePattern: c.TestFileExcludePattern,
		TestFilePattern:        c.TestFilePattern,
		SelectorListPath:       c.SelectorListPath,
		PersistentRunner:       c.persistentRunner,
	}
}

func fromWireSettings(w proto.RunnerSettings, orig RunnerConfig) RunnerConfig {
	resolved := orig
	if w.TestRunner != "" {
		resolved.TestRunner = w.TestRunner
	}
	resolved.locationPrefix = w.LocationPrefix
	resolved.ResultPath = w.ResultPath
	resolved.RetryTestCommand = w.RetryTestCommand
	resolved.TagFilters = w.TagFilters
	resolved.TestCommand = w.TestCommand
	resolved.TestFileExcludePattern = w.TestFileExcludePattern
	resolved.TestFilePattern = w.TestFilePattern
	resolved.SelectorListPath = w.SelectorListPath
	resolved.persistentRunner = w.PersistentRunner
	return resolved
}

func fromWireFeatures(f proto.SupportedFeatures) SupportedFeatures {
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

func toWireTestCases(cases []plan.TestCase) []proto.TestCase {
	out := make([]proto.TestCase, len(cases))
	for i, tc := range cases {
		out[i] = proto.TestCase{
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

func fromWireTestCase(tc proto.TestCase) plan.TestCase {
	return plan.TestCase{
		Format:     plan.TestCaseFormat(tc.Format),
		Identifier: tc.Identifier,
		Name:       tc.Name,
		Path:       tc.Path,
		Scope:      tc.Scope,
		Value:      tc.Value,
	}
}

func fromWireTestCases(cases []proto.TestCase) []plan.TestCase {
	out := make([]plan.TestCase, len(cases))
	for i, tc := range cases {
		out[i] = fromWireTestCase(tc)
	}
	return out
}
