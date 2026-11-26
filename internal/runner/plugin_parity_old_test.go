package runner

import (
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
)

// The runner implementations that the runner plugins replace. While both
// exist, TestPluginParity uses them as the reference for every variant.

func init() { parityVariants = append([]string{"old"}, parityVariants...) }

type oldParityRunner struct{ TestRunnerWithTargetDiscovery }

// batchReportFormat is how `bktest pool run` chooses the report format, for
// the runners it accepts.
func (r oldParityRunner) batchReportFormat() string {
	switch r.TestRunnerWithTargetDiscovery.(type) {
	case Rspec, Jest, Playwright:
		return r.ResultFormat()
	case Pytest:
		if r.ResultFormat() == "junit" {
			return "pytest-junit"
		}
		return "pytest-json"
	}
	return ""
}

func (r oldParityRunner) parseBatchReport(format string, data []byte) (ParsedReport, error) {
	return ParseNativeReport(format, data)
}

func (r oldParityRunner) getExamples(files []string) ([]plan.TestCase, error) {
	return r.TestRunnerWithTargetDiscovery.(ExampleDiscoverer).GetExamples(files)
}

func oldBuiltinRunner(t *testing.T, name string, rc RunnerConfig) (parityRunner, error) {
	t.Helper()
	var r TestRunnerWithTargetDiscovery
	switch name {
	case "rspec":
		r = NewRspec(rc)
	case "jest":
		r = NewJest(rc)
	case "vitest":
		r = NewVitest(rc)
	case "cypress":
		r = NewCypress(rc)
	case "playwright":
		r = NewPlaywright(rc)
	case "pytest":
		r = NewPytest(rc)
	case "gotest":
		r = NewGoTest(rc)
	case "cucumber":
		r = NewCucumber(rc)
	case "custom":
		custom, err := NewCustom(rc)
		if err != nil {
			return nil, err
		}
		r = custom
	default:
		t.Fatalf("unknown runner %q", name)
	}
	return oldParityRunner{r}, nil
}
