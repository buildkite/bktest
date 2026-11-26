package runner

import "github.com/buildkite/bktest/v3/internal/plan"

type TestRunner interface {
	Run(result *RunResult, testCases []plan.TestCase, retry bool) error
	Name() string
	CommandNameAndArgs(testCases []plan.TestCase, retry bool) (string, []string, error)
	LocationPrefix() string
	SupportedFeatures() SupportedFeatures
	UploadToken() string
	// ResultFormat returns the format identifier for the runner's raw result file
	// (e.g. "rspec-json"), used when uploading results to Test Engine.
	// Returns an empty string if the runner does not support raw result uploads.
	ResultFormat() string
	// ResultFilePath returns the path to the runner's raw result file.
	ResultFilePath() string
}

type TestTargetDiscoverer interface {
	DiscoverTestTargets() ([]string, error)
}

type ExampleDiscoverer interface {
	GetExamples(files []string) ([]plan.TestCase, error)
}

type TestRunnerWithTargetDiscovery interface {
	TestRunner
	TestTargetDiscoverer
}

// RunnerIDProvider optionally supplies the canonical runner identifier
// (e.g. "rspec", "pytest") for API requests when loaded via a plugin path.
type RunnerIDProvider interface {
	RunnerID() string
}

// BatchReportRunner optionally describes how a runner's output is interpreted
// in Test Scheduler pool consume mode.
type BatchReportRunner interface {
	BatchReportFormat() string
	PoolNonRetryableExitCodes() bool
}
