// Package runnerplugin defines the runner plugin API: the operations bktest
// asks a test runner integration to perform, the data they exchange, and a
// small SDK for implementing them in Go.
//
// bktest owns orchestration: Test Engine requests, plans, retries, muting,
// process supervision and the job's exit status. A plugin owns framework
// knowledge: its defaults, which targets and examples exist, which command
// runs a set of test cases, and what a finished command's exit status and
// report mean. bktest runs the command a plugin prepares itself, so signal
// forwarding and output streaming are the same for every plugin.
//
// A plugin is a [Handler]. bktest's built-in runners are handlers it calls in
// its own process. An external plugin is an executable that bktest starts
// once per operation, as `<command> <operation>`, with the request as JSON on
// stdin and the response as JSON on stdout; [Serve] implements that for a
// handler, and a script can implement it without this package. The request
// and response types here are the wire format.
//
// Handlers keep nothing in memory between operations: bktest creates a new
// handler, or starts a new process, for each one. Each request carries the
// configuration that Configure resolved, and the path of a directory that
// lasts for the bktest command, where a plugin keeps anything else it needs
// later, such as the exact test names Vitest needs for retries.
//
// See docs/runner-plugins.md for the protocol reference.
package runnerplugin

// ProtocolVersion is the version of the runner plugin API in this package.
// bktest refuses an external plugin that answers with another version.
const ProtocolVersion = 1

// Operations, as passed to an external plugin's command.
const (
	// OpConfigure applies the plugin's defaults to the user's settings and
	// describes the plugin. bktest calls it first, once.
	OpConfigure = "configure"
	// OpPrepareAttempt returns the command that runs a set of test cases.
	OpPrepareAttempt = "prepare-attempt"
	// OpDiscoverTargets returns the files, packages or other targets to plan.
	// Without it, bktest finds files with the configured file patterns.
	OpDiscoverTargets = "discover-targets"
	// OpDiscoverExamples returns the examples in a set of files, for split by
	// example and tag filtering.
	OpDiscoverExamples = "discover-examples"
	// OpInterpretAttempt turns a finished command into test outcomes. Without
	// it, only the command's exit status counts and nothing is retried.
	OpInterpretAttempt = "interpret-attempt"
	// OpParseBatchReport reads a report for Test Scheduler pools.
	OpParseBatchReport = "parse-batch-report"
)

// Config is the runner configuration. Configure receives what the user set,
// where an empty value means unset, and returns it with the plugin's defaults
// applied. Every later request carries the returned configuration.
type Config struct {
	TestCommand            string `json:"test_command,omitempty"`
	RetryTestCommand       string `json:"retry_test_command,omitempty"`
	TestFilePattern        string `json:"test_file_pattern,omitempty"`
	TestFileExcludePattern string `json:"test_file_exclude_pattern,omitempty"`
	ResultPath             string `json:"result_path,omitempty"`
	LocationPrefix         string `json:"location_prefix,omitempty"`
	TagFilters             string `json:"tag_filters,omitempty"`
	SelectorListPath       string `json:"selector_list_path,omitempty"`
	// PersistentRunner is set by `bktest pool run -- <runner>`, where a
	// persistent runner process runs the tests instead of the test command.
	PersistentRunner bool `json:"persistent_runner,omitempty"`
	// Debug is set when bktest runs with --debug. Plugins log to stderr.
	Debug bool `json:"debug,omitempty"`
}

// TestCase mirrors the test cases in bktest's test plans. Path is what the
// framework can run. Scope and Name identify the test the way the Buildkite
// test collector for the framework does, which muting relies on.
type TestCase struct {
	Format     string `json:"format,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Name       string `json:"name,omitempty"`
	Path       string `json:"path"`
	Scope      string `json:"scope,omitempty"`
	// Value is the runnable selector of a selector test case.
	Value string `json:"value,omitempty"`
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

type ConfigureRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Config          Config `json:"config"`
	StateDir        string `json:"state_dir"`
}

type ConfigureResponse struct {
	ProtocolVersion int `json:"protocol_version"`
	// Name is shown in bktest's output, such as "RSpec".
	Name string `json:"name"`
	// Config is the request's configuration with the plugin's defaults.
	Config   Config   `json:"config"`
	Features Features `json:"features"`
	// ResultFormat is the format bktest uploads the report at the result
	// path with when result uploads are enabled, or empty for none.
	ResultFormat string `json:"result_format,omitempty"`
	// BatchReportFormat is the format ParseBatchReport reads the report at
	// the result path with, when it differs from ResultFormat.
	BatchReportFormat string `json:"batch_report_format,omitempty"`
	// Operations are the optional operations the plugin implements. Serve
	// fills it in from the interfaces the handler implements.
	Operations []string `json:"operations,omitempty"`
}

type DiscoverTargetsRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Config          Config `json:"config"`
	StateDir        string `json:"state_dir"`
}

// DiscoverTargetsResponse has either the targets, or file patterns that
// bktest finds files with, using its own matcher.
type DiscoverTargetsResponse struct {
	ProtocolVersion int      `json:"protocol_version"`
	Targets         []string `json:"targets,omitempty"`
	Pattern         string   `json:"pattern,omitempty"`
	ExcludePattern  string   `json:"exclude_pattern,omitempty"`
}

type DiscoverExamplesRequest struct {
	ProtocolVersion int      `json:"protocol_version"`
	Config          Config   `json:"config"`
	StateDir        string   `json:"state_dir"`
	Files           []string `json:"files"`
}

type DiscoverExamplesResponse struct {
	ProtocolVersion int        `json:"protocol_version"`
	TestCases       []TestCase `json:"test_cases"`
}

type PrepareAttemptRequest struct {
	ProtocolVersion int        `json:"protocol_version"`
	Config          Config     `json:"config"`
	StateDir        string     `json:"state_dir"`
	TestCases       []TestCase `json:"test_cases"`
	// Retry is set when the test cases are failed tests being retried.
	Retry bool `json:"retry"`
}

type PrepareAttemptResponse struct {
	ProtocolVersion int `json:"protocol_version"`
	// Argv is the command bktest runs, without a shell.
	Argv []string `json:"argv"`
	// StdoutPath asks bktest to also write the command's stdout to this
	// file, such as `go test -json` output that is the report.
	StdoutPath string `json:"stdout_path,omitempty"`
}

type InterpretAttemptRequest struct {
	ProtocolVersion int        `json:"protocol_version"`
	Config          Config     `json:"config"`
	StateDir        string     `json:"state_dir"`
	TestCases       []TestCase `json:"test_cases"`
	Retry           bool       `json:"retry"`
	// ExitCode is the command's exit code, or nil if it didn't exit
	// normally, such as when it couldn't start.
	ExitCode *int `json:"exit_code"`
	// Error describes why the command failed, if it did.
	Error string `json:"error,omitempty"`
}

// TestResult is the outcome of one test in an attempt.
type TestResult struct {
	TestCase TestCase `json:"test_case"`
	Status   string   `json:"status"`
	// Key identifies the test across attempts. The default is scope, name
	// and path. RSpec uses its example ID because descriptions can repeat.
	Key string `json:"key,omitempty"`
}

// InterpretAttemptResponse reports what happened. bktest decides what to
// retry and how the job ends; the attempt's error stays the command's own.
type InterpretAttemptResponse struct {
	ProtocolVersion int `json:"protocol_version"`
	// Messages are printed to bktest's stdout, in order, before the results
	// are recorded.
	Messages []string     `json:"messages,omitempty"`
	Tests    []TestResult `json:"tests,omitempty"`
	// RunError is an error outside the tests, such as a build failure, that
	// retrying the failed tests can't fix. It ends the run with an error.
	RunError string `json:"run_error,omitempty"`
	// ErroredFiles failed outside their tests, such as files that didn't
	// load. They error the run, but other failed tests are still retried.
	ErroredFiles []string `json:"errored_files,omitempty"`
	// Rerun asks bktest to run these test cases again in the same attempt,
	// such as the files Playwright didn't run because another file failed
	// to load.
	Rerun []TestCase `json:"rerun,omitempty"`
	// FailedTestsMessage fails the attempt with this message when the
	// command exited 0 but the run has failed tests, as Vitest can.
	FailedTestsMessage string `json:"failed_tests_message,omitempty"`
}

type ParseBatchReportRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Config          Config `json:"config"`
	StateDir        string `json:"state_dir"`
	// Format is the report's format, such as "rspec-json".
	Format string `json:"format"`
	// ReportPath is a file with the report's contents.
	ReportPath string `json:"report_path"`
}

// BatchTest is a test in a pool batch report.
type BatchTest struct {
	TestCase TestCase `json:"test_case"`
	Status   string   `json:"status"`
	// Selector is the leased selector the test belongs to, such as its file.
	Selector string `json:"selector,omitempty"`
	// Location is the test's file and line, when the report has it.
	Location string `json:"location,omitempty"`
}

// ParseBatchReportResponse mirrors what bktest's pool accounting needs from a
// report. It follows bktest's internals more closely than the other
// operations and is the most likely part of the API to change.
type ParseBatchReportResponse struct {
	ProtocolVersion int         `json:"protocol_version"`
	Tests           []BatchTest `json:"tests,omitempty"`
	// ErrorsOutsideTests is set when the report has errors that belong to
	// no test, such as RSpec's errors outside of examples.
	ErrorsOutsideTests bool `json:"errors_outside_tests,omitempty"`
	// ErroredFiles are the files that failed outside their tests.
	ErroredFiles []string `json:"errored_files,omitempty"`
	// ErroredFilesStoppedRun is set when the errored files stopped every
	// other test from running, as a Playwright file that fails to load does.
	ErroredFilesStoppedRun bool `json:"errored_files_stopped_run,omitempty"`
	// UnselectedTests is set when the report can include tests the command
	// wasn't asked to run, such as Playwright's setup projects.
	UnselectedTests bool `json:"unselected_tests,omitempty"`
}
