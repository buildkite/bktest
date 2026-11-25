// Package runnerplugin defines the protocol bktest uses to talk to test runner
// plugins, and a small SDK for writing plugins in Go.
//
// A runner plugin is an executable that bktest starts as a child process, in
// the style of a Terraform provider: one long-lived process per bktest
// command, speaking a versioned protocol. Unlike Terraform, the transport is
// newline-delimited JSON-RPC 2.0 over the plugin's stdin and stdout, so a Bash
// script with jq can implement the same protocol as a compiled Go binary. The
// plugin's stderr is passed through to bktest's stderr for logs.
//
// bktest owns orchestration: Test Engine requests, plans, retries, muting,
// process supervision and the job's exit status. A plugin owns framework
// knowledge: which targets exist, which examples a file contains, which argv
// runs a set of test cases, and what a finished command's exit status and
// report mean. bktest runs the argv returned by "command" itself, so signal
// forwarding and output streaming behave the same for every plugin.
//
// See docs/runner-plugins.md for the protocol reference.
package runnerplugin

// ProtocolVersion is the protocol version this package implements. bktest
// sends it in "initialize" and refuses a plugin that answers with another.
const ProtocolVersion = 1

// ProtocolEnv is set by bktest when it starts a plugin. A plugin started
// without it is being run by hand, and should explain itself and exit.
const ProtocolEnv = "BKTEST_RUNNER_PLUGIN_PROTOCOL"

// Methods bktest calls, in the order it calls them.
const (
	// MethodInitialize sends the runner configuration and returns the
	// plugin's identity, features and effective configuration.
	MethodInitialize = "initialize"
	// MethodDiscover returns the targets (files, packages, ...) to plan.
	MethodDiscover = "discover"
	// MethodExamples returns the examples in the given files, for split by
	// example and tag filtering.
	MethodExamples = "examples"
	// MethodCommand returns the argv bktest runs for a set of test cases.
	MethodCommand = "command"
	// MethodInterpret turns a finished command into test outcomes.
	MethodInterpret = "interpret"
)

// TestCase mirrors the test case bktest receives in test plans. Path is what
// the framework can run; Scope and Name identify the test like the Buildkite
// test collector for the framework does, which muting relies on.
type TestCase struct {
	EstimatedDuration int    `json:"estimated_duration,omitempty"`
	Format            string `json:"format,omitempty"`
	Identifier        string `json:"identifier,omitempty"`
	Name              string `json:"name,omitempty"`
	Path              string `json:"path"`
	Scope             string `json:"scope,omitempty"`
	Value             string `json:"value,omitempty"`
}

// Test case formats.
const (
	FormatFile     = "file"
	FormatExample  = "example"
	FormatSelector = "selector"
)

// Test statuses a plugin can report.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	// StatusNotRun is a test that was expected to run but did not, such as a
	// test skipped because an earlier test in its serial group failed.
	StatusNotRun  = "not_run"
	StatusUnknown = "unknown"
)

// Config is the runner configuration from bktest's flags and environment.
// Empty values mean the user didn't set them; the plugin applies defaults.
type Config struct {
	TestCommand            string `json:"test_command,omitempty"`
	RetryTestCommand       string `json:"retry_test_command,omitempty"`
	TestFilePattern        string `json:"test_file_pattern,omitempty"`
	TestFileExcludePattern string `json:"test_file_exclude_pattern,omitempty"`
	ResultPath             string `json:"result_path,omitempty"`
	LocationPrefix         string `json:"location_prefix,omitempty"`
	TagFilters             string `json:"tag_filters,omitempty"`
	SelectorListPath       string `json:"selector_list_path,omitempty"`
	Debug                  bool   `json:"debug,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion int    `json:"protocol_version"`
	Config          Config `json:"config"`
}

// Features are the bktest features the runner supports.
type Features struct {
	SplitByFile     bool `json:"split_by_file"`
	SplitByExample  bool `json:"split_by_example"`
	SplitBySelector bool `json:"split_by_selector"`
	FilterTestFiles bool `json:"filter_test_files"`
	FilterTestByTag bool `json:"filter_test_by_tag"`
	AutoRetry       bool `json:"auto_retry"`
	Mute            bool `json:"mute"`
	Skip            bool `json:"skip"`
}

type InitializeResult struct {
	ProtocolVersion int `json:"protocol_version"`
	// Name is shown in bktest's output, such as "RSpec".
	Name     string   `json:"name"`
	Features Features `json:"features"`
	// ResultFormat is the format of the report at ResultPath that bktest
	// uploads to Test Engine when upload is enabled, or empty for none.
	ResultFormat string `json:"result_format,omitempty"`
	// ResultPath is the effective result path, after defaults.
	ResultPath string `json:"result_path,omitempty"`
	// LocationPrefix is the effective location prefix, after defaults.
	LocationPrefix string `json:"location_prefix,omitempty"`
}

type DiscoverResult struct {
	Targets []string `json:"targets"`
}

type ExamplesParams struct {
	Files []string `json:"files"`
}

type ExamplesResult struct {
	TestCases []TestCase `json:"test_cases"`
}

type CommandParams struct {
	TestCases []TestCase `json:"test_cases"`
	Retry     bool       `json:"retry"`
}

type CommandResult struct {
	Argv []string `json:"argv"`
	// StdoutPath asks bktest to also write the command's stdout to this file,
	// such as `go test -json` output that is the report.
	StdoutPath string `json:"stdout_path,omitempty"`
}

type InterpretParams struct {
	TestCases []TestCase `json:"test_cases"`
	Retry     bool       `json:"retry"`
	// ExitCode is the command's exit code, or nil if it didn't exit normally,
	// such as when it couldn't start.
	ExitCode *int `json:"exit_code"`
	// Error describes why the command failed, if it did.
	Error string `json:"error,omitempty"`
}

type TestResult struct {
	TestCase TestCase `json:"test_case"`
	Status   string   `json:"status"`
	// Key identifies the test across attempts. The default is scope, name
	// and path; RSpec uses its example ID because descriptions can repeat.
	Key string `json:"key,omitempty"`
}

type InterpretResult struct {
	// Messages are printed to bktest's stdout, in order, before the results
	// are recorded.
	Messages []string     `json:"messages,omitempty"`
	Tests    []TestResult `json:"tests,omitempty"`
	// Error is an error outside the tests, such as a build failure, that
	// retrying the failed tests can't fix. It ends the run with an error.
	Error string `json:"error,omitempty"`
	// ErroredFiles failed outside their tests, such as files that didn't
	// load. They error the run, but other failed tests are still retried.
	ErroredFiles []string `json:"errored_files,omitempty"`
	// Rerun asks bktest to run these test cases again in the same attempt,
	// such as the files Playwright didn't run because another file failed to
	// load. bktest runs them like any other command and interprets them.
	Rerun []TestCase `json:"rerun,omitempty"`
	// FailedTestsMessage fails the attempt with this message when the
	// command exited 0 but the run has failed tests, as Vitest can.
	FailedTestsMessage string `json:"failed_tests_message,omitempty"`
}
