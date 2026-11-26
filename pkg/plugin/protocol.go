// Package plugin defines the wire contract and Go SDK for external bktest
// runner plugins. Both compiled Go executables and scripted plugins (such as
// Bash) implement the same versioned JSON-over-stdio subcommand protocol.
package plugin

import "encoding/json"

// ProtocolVersion is the current wire protocol version spoken between bktest
// and external runner plugins.
const ProtocolVersion = 1

// Supported plugin operations invoked as `<plugin-executable> <operation>`.
const (
	OpConfigure        = "configure"
	OpDiscoverTargets  = "discover-targets"
	OpDiscoverExamples = "discover-examples"
	OpPrepareAttempt   = "prepare-attempt"
	OpInterpretAttempt = "interpret-attempt"
	OpParseBatchReport = "parse-batch-report"
)

// TestStatus values returned by plugins in attempt and batch report outcomes.
const (
	TestStatusPassed  = "passed"
	TestStatusFailed  = "failed"
	TestStatusSkipped = "skipped"
	TestStatusNotRun  = "not_run"
	TestStatusUnknown = "unknown"
)

// TestCaseFormat values matching internal/plan.TestCaseFormat.
const (
	TestCaseFormatFile     = "file"
	TestCaseFormatExample  = "example"
	TestCaseFormatSelector = "selector"
)

// Manifest describes a plugin located in a directory (plugin.json).
type Manifest struct {
	ProtocolVersion int        `json:"protocol_version"`
	Name            string     `json:"name"`
	DisplayName     string     `json:"display_name,omitempty"`
	Kind            string     `json:"kind,omitempty"`
	Entrypoint      string     `json:"entrypoint"`
	Build           *BuildSpec `json:"build,omitempty"`
	ReportFormats   []string   `json:"report_formats,omitempty"`
}

// BuildSpec optionally tells bktest how to compile a source-based Go plugin
// into a cached executable binary when the prebuilt binary is not yet on disk.
type BuildSpec struct {
	GoPackage string `json:"go_package,omitempty"`
}

// RunnerSettings carries the user-supplied and resolved runner configuration.
type RunnerSettings struct {
	TestRunner             string `json:"test_runner,omitempty"`
	LocationPrefix         string `json:"location_prefix,omitempty"`
	ResultPath             string `json:"result_path,omitempty"`
	RetryTestCommand       string `json:"retry_test_command,omitempty"`
	TagFilters             string `json:"tag_filters,omitempty"`
	TestCommand            string `json:"test_command,omitempty"`
	TestFileExcludePattern string `json:"test_file_exclude_pattern,omitempty"`
	TestFilePattern        string `json:"test_file_pattern,omitempty"`
	SelectorListPath       string `json:"selector_list_path,omitempty"`
	PersistentRunner       bool   `json:"persistent_runner,omitempty"`
}

// SupportedFeatures mirrors runner.SupportedFeatures over the wire.
type SupportedFeatures struct {
	SplitByFile     bool `json:"split_by_file"`
	SplitByExample  bool `json:"split_by_example"`
	SplitBySelector bool `json:"split_by_selector"`
	FilterTestFiles bool `json:"filter_test_files"`
	FilterTestByTag bool `json:"filter_test_by_tag"`
	AutoRetry       bool `json:"auto_retry"`
	Mute            bool `json:"mute"`
	Skip            bool `json:"skip"`
}

// Capabilities declares which optional operations a plugin implements.
type Capabilities struct {
	DiscoverTargets  bool `json:"discover_targets"`
	DiscoverExamples bool `json:"discover_examples"`
	InterpretAttempt bool `json:"interpret_attempt"`
	ParseBatchReport bool `json:"parse_batch_report"`
}

// TestCase is the wire representation of a test file, example, or selector.
type TestCase struct {
	Format     string `json:"format,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Name       string `json:"name,omitempty"`
	Path       string `json:"path,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Value      string `json:"value,omitempty"`
}

// ConfigureRequest is sent to `<plugin> configure`.
type ConfigureRequest struct {
	ProtocolVersion int            `json:"protocol_version"`
	WorkingDir      string         `json:"working_dir"`
	Config          RunnerSettings `json:"config"`
}

// ConfigureResponse is returned by `<plugin> configure`.
type ConfigureResponse struct {
	ProtocolVersion           int               `json:"protocol_version"`
	RunnerID                  string            `json:"runner_id"`
	Name                      string            `json:"name"`
	Config                    RunnerSettings    `json:"config"`
	ResultFormat              string            `json:"result_format,omitempty"`
	BatchReportFormat         string            `json:"batch_report_format,omitempty"`
	RequiresResultPath        bool              `json:"requires_result_path,omitempty"`
	PoolNonRetryableExitCodes bool              `json:"pool_non_retryable_exit_codes,omitempty"`
	Features                  SupportedFeatures `json:"features"`
	Capabilities              Capabilities      `json:"capabilities"`
	Warnings                  []string          `json:"warnings,omitempty"`
	State                     json.RawMessage   `json:"state,omitempty"`
	Error                     string            `json:"error,omitempty"`
}

// DiscoverTargetsRequest is sent to `<plugin> discover-targets`.
type DiscoverTargetsRequest struct {
	ProtocolVersion int             `json:"protocol_version"`
	WorkingDir      string          `json:"working_dir"`
	Config          RunnerSettings  `json:"config"`
	State           json.RawMessage `json:"state,omitempty"`
}

// DiscoverTargetsResponse is returned by `<plugin> discover-targets`.
type DiscoverTargetsResponse struct {
	ProtocolVersion int      `json:"protocol_version"`
	Targets         []string `json:"targets"`
	Error           string   `json:"error,omitempty"`
}

// DiscoverExamplesRequest is sent to `<plugin> discover-examples`.
type DiscoverExamplesRequest struct {
	ProtocolVersion int             `json:"protocol_version"`
	WorkingDir      string          `json:"working_dir"`
	Config          RunnerSettings  `json:"config"`
	Files           []string        `json:"files"`
	State           json.RawMessage `json:"state,omitempty"`
}

// DiscoverExamplesResponse is returned by `<plugin> discover-examples`.
type DiscoverExamplesResponse struct {
	ProtocolVersion int        `json:"protocol_version"`
	Examples        []TestCase `json:"examples"`
	Error           string     `json:"error,omitempty"`
}

// PrepareAttemptRequest is sent to `<plugin> prepare-attempt`.
type PrepareAttemptRequest struct {
	ProtocolVersion int             `json:"protocol_version"`
	WorkingDir      string          `json:"working_dir"`
	Config          RunnerSettings  `json:"config"`
	TestCases       []TestCase      `json:"test_cases"`
	Retry           bool            `json:"retry"`
	State           json.RawMessage `json:"state,omitempty"`
}

// PrepareAttemptResponse is returned by `<plugin> prepare-attempt`.
type PrepareAttemptResponse struct {
	ProtocolVersion int               `json:"protocol_version"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env,omitempty"`
	StdoutTeePath   string            `json:"stdout_tee_path,omitempty"`
	State           json.RawMessage   `json:"state,omitempty"`
	Error           string            `json:"error,omitempty"`
}

// InterpretAttemptRequest is sent to `<plugin> interpret-attempt`.
type InterpretAttemptRequest struct {
	ProtocolVersion int             `json:"protocol_version"`
	WorkingDir      string          `json:"working_dir"`
	Config          RunnerSettings  `json:"config"`
	TestCases       []TestCase      `json:"test_cases"`
	Retry           bool            `json:"retry"`
	ExitCode        int             `json:"exit_code"`
	CommandFailed   bool            `json:"command_failed"`
	Signaled        bool            `json:"signaled"`
	Signal          int             `json:"signal,omitempty"`
	CommandError    string          `json:"command_error,omitempty"`
	State           json.RawMessage `json:"state,omitempty"`
}

// AttemptTestOutcome is a single test result emitted by interpret-attempt.
type AttemptTestOutcome struct {
	Key      string   `json:"key,omitempty"`
	TestCase TestCase `json:"test_case"`
	Status   string   `json:"status"`
}

// InterpretAttemptResponse is returned by `<plugin> interpret-attempt`.
type InterpretAttemptResponse struct {
	ProtocolVersion           int                  `json:"protocol_version"`
	Results                   []AttemptTestOutcome `json:"results,omitempty"`
	ErroredFiles              []string             `json:"errored_files,omitempty"`
	RunError                  string               `json:"run_error,omitempty"`
	UseCommandErrorAsRunError bool                 `json:"use_command_error_as_run_error,omitempty"`
	RerunTestCases            []TestCase           `json:"rerun_test_cases,omitempty"`
	RerunMessage              string               `json:"rerun_message,omitempty"`
	FailIfFailedTests         bool                 `json:"fail_if_failed_tests,omitempty"`
	FailIfFailedTestsMessage  string               `json:"fail_if_failed_tests_message,omitempty"`
	ParseWarning              string               `json:"parse_warning,omitempty"`
	State                     json.RawMessage      `json:"state,omitempty"`
	Error                     string               `json:"error,omitempty"`
}

// ParseBatchReportRequest is sent to `<plugin> parse-batch-report`.
type ParseBatchReportRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	WorkingDir      string `json:"working_dir"`
	Format          string `json:"format"`
	ReportBase64    string `json:"report_base64"`
}

// BatchReportedTest mirrors runner.ReportedTest over the wire.
type BatchReportedTest struct {
	TestCase TestCase `json:"test_case"`
	Status   string   `json:"status"`
	Selector string   `json:"selector,omitempty"`
	Location string   `json:"location,omitempty"`
}

// ParseBatchReportResponse is returned by `<plugin> parse-batch-report`.
type ParseBatchReportResponse struct {
	ProtocolVersion        int                 `json:"protocol_version"`
	Tests                  []BatchReportedTest `json:"tests,omitempty"`
	ErrorsOutsideTests     bool                `json:"errors_outside_tests,omitempty"`
	ErroredFiles           []string            `json:"errored_files,omitempty"`
	ErroredFilesStoppedRun bool                `json:"errored_files_stopped_run,omitempty"`
	UnselectedTests        bool                `json:"unselected_tests,omitempty"`
	Error                  string              `json:"error,omitempty"`
}
