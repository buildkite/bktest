package testjson

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
)

const pkg = "example.com/t2j/p"

// ev builds one go test -json line. Times are seconds after a fixed epoch so
// tests can assert on durations.
func ev(sec float64, action, test, output string, extra ...string) string {
	t := time.Date(2026, 10, 18, 5, 0, 0, 0, time.UTC).Add(time.Duration(sec * float64(time.Second)))
	s := fmt.Sprintf(`{"Time":%q,"Action":%q,"Package":%q`, t.Format(time.RFC3339Nano), action, pkg)
	if test != "" {
		s += fmt.Sprintf(`,"Test":%q`, test)
	}
	if output != "" {
		s += fmt.Sprintf(`,"Output":%q`, output)
	}
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

type recorded struct {
	executions   []Execution
	started      []string
	failedBefore []string
	exported     []string
	echoed       []string
	nonEvents    int
}

func parse(t *testing.T, lines ...string) (*Parser, *recorded) {
	t.Helper()
	r := &recorded{}
	p := NewParser(Handler{
		Execution:                func(e Execution) { r.executions = append(r.executions, e) },
		PackageStarted:           func(s string) { r.started = append(r.started, s) },
		PackageFailedBeforeTests: func(s string) { r.failedBefore = append(r.failedBefore, s) },
		LibraryExported:          func(s string) { r.exported = append(r.exported, s) },
	})
	for _, line := range lines {
		out, ok := p.Line([]byte(line))
		if !ok {
			r.nonEvents++
			continue
		}
		if out != "" {
			r.echoed = append(r.echoed, out)
		}
	}
	p.Finish()
	return p, r
}

func (r *recorded) byName(t *testing.T, name string) Execution {
	t.Helper()
	for _, e := range r.executions {
		if e.Test == name {
			return e
		}
	}
	t.Fatalf("no execution named %q in %+v", name, r.executions)
	return Execution{}
}

func TestPassFailSkipBecomeExecutionsWithTheirOutputEchoed(t *testing.T) {
	p, r := parse(t,
		ev(0, "start", "", ""),
		ev(0.2, "run", "TestPass", ""),
		ev(0.2, "output", "TestPass", "=== RUN   TestPass\n"),
		ev(0.2, "output", "TestPass", "    p_test.go:16: hello\n"),
		ev(0.2, "output", "TestPass", "--- PASS: TestPass (0.00s)\n"),
		ev(0.21, "pass", "TestPass", "", `"Elapsed":0.01`),
		ev(0.3, "run", "TestFail", ""),
		ev(0.3, "output", "TestFail", "=== RUN   TestFail\n"),
		ev(0.3, "output", "TestFail", "    p_test.go:17: deliberate failure\n"),
		ev(0.3, "output", "TestFail", "    p_test.go:17: second\n"),
		ev(0.3, "output", "TestFail", "--- FAIL: TestFail (0.00s)\n"),
		ev(0.3, "fail", "TestFail", "", `"Elapsed":0`),
		ev(0.4, "run", "TestSkip", ""),
		ev(0.4, "output", "TestSkip", "=== RUN   TestSkip\n"),
		ev(0.4, "output", "TestSkip", "    p_test.go:18: because\n"),
		ev(0.4, "output", "TestSkip", "--- SKIP: TestSkip (0.00s)\n"),
		ev(0.4, "skip", "TestSkip", "", `"Elapsed":0`),
		ev(0.5, "output", "", "FAIL\n"),
		ev(0.5, "output", "", "FAIL\texample.com/t2j/p\t0.227s\n"),
		ev(0.5, "fail", "", "", `"Elapsed":0.227`),
	)

	if p.Events() != 20 || !p.Failed() {
		t.Fatalf("events=%d failed=%v", p.Events(), p.Failed())
	}
	if len(r.executions) != 3 {
		t.Fatalf("executions: %+v", r.executions)
	}
	if got := strings.Join(r.started, ","); got != pkg {
		t.Errorf("started = %q", got)
	}
	if len(r.failedBefore) != 0 {
		t.Errorf("package ran tests but was reported as failed before tests")
	}

	pass := r.byName(t, "TestPass")
	if pass.Result.Status != collector.StatusPass || pass.Package != pkg {
		t.Errorf("pass = %+v", pass)
	}
	if d := pass.End.Sub(pass.Start); d != 10*time.Millisecond {
		t.Errorf("duration = %v, want Elapsed", d)
	}

	fail := r.byName(t, "TestFail")
	if fail.Result.Status != collector.StatusFail {
		t.Errorf("fail status = %q", fail.Result.Status)
	}
	if fail.Result.Reason != "p_test.go:17: deliberate failure" {
		t.Errorf("reason = %q", fail.Result.Reason)
	}
	if fail.Result.Exception == nil || fail.Result.Exception.Type != "failure" ||
		fail.Result.Exception.Message != "p_test.go:17: deliberate failure\np_test.go:17: second" {
		t.Errorf("exception = %+v", fail.Result.Exception)
	}

	skip := r.byName(t, "TestSkip")
	if skip.Result.Status != collector.StatusSkipped || skip.Result.Reason != "" {
		t.Errorf("skip = %+v", skip.Result)
	}

	// Every Output event is echoed, framing included, so the CI log reads
	// like go test -v.
	echoed := strings.Join(r.echoed, "")
	for _, want := range []string{"=== RUN   TestPass\n", "    p_test.go:16: hello\n", "--- FAIL: TestFail (0.00s)\n", "FAIL\texample.com/t2j/p\t0.227s\n"} {
		if !strings.Contains(echoed, want) {
			t.Errorf("echoed output lacks %q:\n%s", want, echoed)
		}
	}
}

func TestZeroElapsedFallsBackToEventTimestampsCappedAtOneTick(t *testing.T) {
	_, r := parse(t,
		ev(0.100, "run", "TestFast", ""),
		ev(0.103, "pass", "TestFast", "", `"Elapsed":0`),
		ev(0.200, "run", "TestLate", ""),
		ev(0.250, "pass", "TestLate", "", `"Elapsed":0`),
		ev(0.300, "run", "TestBackwards", ""),
		ev(0.299, "pass", "TestBackwards", "", `"Elapsed":0`),
		ev(0.400, "run", "TestMeasured", ""),
		ev(0.401, "pass", "TestMeasured", "", `"Elapsed":0.05`),
	)
	for name, want := range map[string]time.Duration{
		"TestFast":      3 * time.Millisecond,
		"TestLate":      10 * time.Millisecond,
		"TestBackwards": 0,
		"TestMeasured":  50 * time.Millisecond,
	} {
		e := r.byName(t, name)
		if d := e.End.Sub(e.Start); d != want {
			t.Errorf("%s duration = %v, want %v", name, d, want)
		}
	}
}

func TestSubtestsKeepTheirFullNameAndOwnOutput(t *testing.T) {
	_, r := parse(t,
		ev(0, "run", "TestSub", ""),
		ev(0, "output", "TestSub", "=== RUN   TestSub\n"),
		ev(0, "run", "TestSub/a", ""),
		ev(0, "output", "TestSub/a", "=== RUN   TestSub/a\n"),
		ev(0, "output", "TestSub/a", "=== PAUSE TestSub/a\n"),
		ev(0, "pause", "TestSub/a", ""),
		ev(0, "run", "TestSub/b", ""),
		ev(0, "output", "TestSub/b", "=== RUN   TestSub/b\n"),
		ev(0, "output", "TestSub/b", "=== PAUSE TestSub/b\n"),
		ev(0, "pause", "TestSub/b", ""),
		ev(0, "output", "TestSub", "    p_test.go:22: parent log\n"),
		ev(2, "cont", "TestSub/a", ""),
		ev(2, "output", "TestSub/a", "=== CONT  TestSub/a\n"),
		ev(2, "cont", "TestSub/b", ""),
		ev(2, "output", "TestSub/b", "=== CONT  TestSub/b\n"),
		ev(2, "output", "TestSub/b", "    p_test.go:21: sub failed\n"),
		ev(2, "output", "TestSub/b", "        continued\n"),
		ev(2, "output", "TestSub/b", "--- FAIL: TestSub/b (0.00s)\n"),
		ev(2.01, "fail", "TestSub/b", "", `"Elapsed":0`),
		ev(2.01, "output", "TestSub/a", "--- PASS: TestSub/a (0.01s)\n"),
		ev(2.01, "pass", "TestSub/a", "", `"Elapsed":0.01`),
		ev(2.01, "output", "TestSub", "--- FAIL: TestSub (0.00s)\n"),
		ev(2.01, "fail", "TestSub", "", `"Elapsed":2.01`),
		ev(2.02, "fail", "", "", `"Elapsed":2.1`),
	)

	if len(r.executions) != 3 {
		t.Fatalf("executions: %+v", r.executions)
	}
	b := r.byName(t, "TestSub/b")
	if b.Result.Reason != "p_test.go:21: sub failed" {
		t.Errorf("reason = %q", b.Result.Reason)
	}
	if b.Result.Exception.Message != "p_test.go:21: sub failed\n    continued" {
		t.Errorf("message = %q; want go test's indentation removed", b.Result.Exception.Message)
	}

	// A paused parallel test starts at its run event and lasts Elapsed.
	a := r.byName(t, "TestSub/a")
	if !a.Start.Equal(time.Date(2026, 10, 18, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v, want the run time", a.Start)
	}
	if a.End.Sub(a.Start) != 10*time.Millisecond {
		t.Errorf("duration = %v", a.End.Sub(a.Start))
	}

	parent := r.byName(t, "TestSub")
	if parent.Result.Status != collector.StatusFail || parent.Result.Reason != "p_test.go:22: parent log" {
		t.Errorf("parent = %+v", parent.Result)
	}
}

func TestPanicIsAFailureWithThePanicLineAsReasonAndTheTraceAsStack(t *testing.T) {
	_, r := parse(t,
		ev(0, "run", "TestPanics", ""),
		ev(0, "output", "TestPanics", "=== RUN   TestPanics\n"),
		ev(0, "output", "TestPanics", "    pn_test.go:4: opening database\n"),
		ev(0, "output", "TestPanics", "--- FAIL: TestPanics (0.00s)\n"),
		ev(0, "output", "TestPanics", "panic: assignment to entry in nil map [recovered, repanicked]\n"),
		ev(0, "output", "TestPanics", "\n"),
		ev(0, "output", "TestPanics", "goroutine 34 [running]:\n"),
		ev(0, "output", "TestPanics", "example.com/t2j/pn.TestPanics(0xb3a426d2248?)\n"),
		ev(0, "output", "TestPanics", "\t/tmp/t2j/pn/pn_test.go:5 +0x34\n"),
		ev(0, "fail", "TestPanics", "", `"Elapsed":0`),
		ev(1, "fail", "", "", `"Elapsed":1`),
	)
	e := r.byName(t, "TestPanics")
	if e.Result.Status != collector.StatusFail || e.Result.Reason != "panic: assignment to entry in nil map [recovered, repanicked]" {
		t.Errorf("result = %+v", e.Result)
	}
	if e.Result.Exception == nil || e.Result.Exception.Type != "panic" ||
		e.Result.Exception.Message != "pn_test.go:4: opening database\npanic: assignment to entry in nil map [recovered, repanicked]" ||
		!strings.HasPrefix(e.Result.Exception.Stacktrace, "\ngoroutine 34 [running]:\nexample.com/t2j/pn.TestPanics") {
		t.Errorf("exception = %+v", e.Result.Exception)
	}
}

func TestUserOutputThatLooksLikeFramingIsKeptWhenGoLabelsFrames(t *testing.T) {
	_, r := parse(t,
		ev(0, "run", "TestFail", ""),
		ev(0, "output", "TestFail", "=== RUN   TestFail\n", `"OutputType":"frame"`),
		ev(0, "output", "TestFail", "=== RUN database unavailable\n"),
		ev(0, "output", "TestFail", "--- FAIL: TestFail (0.00s)\n", `"OutputType":"frame"`),
		ev(0, "fail", "TestFail", "", `"Elapsed":0`),
		ev(0, "fail", "", "", `"Elapsed":0`),
	)
	if got := r.byName(t, "TestFail").Result.Reason; got != "=== RUN database unavailable" {
		t.Errorf("reason = %q", got)
	}
}

func TestATestStillRunningWhenItsPackageEndsFails(t *testing.T) {
	_, r := parse(t,
		ev(0, "start", "", ""),
		ev(0.8, "run", "TestSlow", ""),
		ev(0.8, "output", "TestSlow", "=== RUN   TestSlow\n"),
		ev(1.8, "output", "TestSlow", "panic: test timed out after 1s\n"),
		ev(1.8, "output", "TestSlow", "\trunning tests:\n"),
		ev(1.8, "output", "TestSlow", "\t\tTestSlow (1s)\n"),
		ev(1.9, "output", "", "FAIL\texample.com/t2j/q\t1.2s\n"),
		ev(1.9, "fail", "", "", `"Elapsed":1.2`),
	)
	if len(r.executions) != 1 {
		t.Fatalf("executions: %+v", r.executions)
	}
	e := r.executions[0]
	if e.Result.Status != collector.StatusFail || e.Result.Reason != "panic: test timed out after 1s" {
		t.Errorf("result = %+v", e.Result)
	}
	if d := e.End.Sub(e.Start); d != 1100*time.Millisecond {
		t.Errorf("duration = %v, want run → package end", d)
	}
	if len(r.failedBefore) != 0 {
		t.Errorf("a package that ran a test is not a build failure")
	}
}

func TestATestStillRunningAtEndOfInputFails(t *testing.T) {
	p, r := parse(t,
		ev(0, "run", "TestKilled", ""),
		ev(0, "output", "TestKilled", "=== RUN   TestKilled\n"),
	)
	if len(r.executions) != 1 || r.executions[0].Result.Status != collector.StatusFail || r.executions[0].Result.Reason != "test failed" {
		t.Errorf("executions: %+v", r.executions)
	}
	if !p.Failed() {
		t.Error("Failed() = false")
	}
}

func TestBuildFailureReportsThePackageAndNoExecutions(t *testing.T) {
	p, r := parse(t,
		`{"ImportPath":"example.com/t2j/p [example.com/t2j/p.test]","Action":"build-output","Output":"# example.com/t2j/p\n"}`,
		`{"ImportPath":"example.com/t2j/p [example.com/t2j/p.test]","Action":"build-output","Output":"p_test.go:5:28: undefined: undefined\n"}`,
		`{"ImportPath":"example.com/t2j/p [example.com/t2j/p.test]","Action":"build-fail"}`,
		ev(0, "start", "", ""),
		ev(0, "output", "", "FAIL\texample.com/t2j/p [build failed]\n"),
		ev(0, "fail", "", "", `"Elapsed":0,"FailedBuild":"example.com/t2j/p [example.com/t2j/p.test]"`),
	)
	if len(r.executions) != 0 || !p.Failed() {
		t.Errorf("executions=%+v failed=%v", r.executions, p.Failed())
	}
	if strings.Join(r.failedBefore, ",") != pkg || strings.Join(r.started, ",") != pkg {
		t.Errorf("failedBefore=%v started=%v; build events must not register a package", r.failedBefore, r.started)
	}
	if r.nonEvents != 0 || p.Events() != 6 {
		t.Errorf("nonEvents=%d events=%d", r.nonEvents, p.Events())
	}
	if !strings.Contains(strings.Join(r.echoed, ""), "p_test.go:5:28: undefined: undefined\n") {
		t.Errorf("compiler output not echoed: %q", r.echoed)
	}
}

func TestPackageFailingWithoutRunningTestsIsReportedOnOlderGo(t *testing.T) {
	_, r := parse(t,
		ev(0, "start", "", ""),
		ev(0, "output", "", "FAIL\texample.com/t2j/p [build failed]\n"),
		ev(0, "fail", "", "", `"Elapsed":0`),
	)
	if strings.Join(r.failedBefore, ",") != pkg {
		t.Errorf("failedBefore = %v", r.failedBefore)
	}
}

// go test -bench gives a benchmark a run event and, when it succeeds, no
// terminal event at all; it must not be counted as a test that never
// finished.
func TestBenchmarksAndPackagePassAreNotExecutions(t *testing.T) {
	p, r := parse(t,
		ev(0, "start", "", ""),
		ev(0, "output", "", "goos: darwin\n"),
		ev(0, "run", "BenchmarkX", ""),
		ev(0, "output", "BenchmarkX", "=== RUN   BenchmarkX\n"),
		ev(0, "output", "BenchmarkX", "BenchmarkX\n"),
		ev(0, "output", "BenchmarkX", "BenchmarkX-8   \t1000000\t 1000 ns/op\n"),
		ev(0, "run", "BenchmarkX/sub", ""),
		ev(0, "output", "BenchmarkX/sub", "BenchmarkX/sub-8   \t1000000\t 1000 ns/op\n"),
		ev(0, "bench", "BenchmarkY", "", `"Elapsed":0`),
		ev(0, "output", "", "ok  \texample.com/t2j/p\t1.0s\n"),
		ev(0, "pass", "", "", `"Elapsed":1`),
	)
	if len(r.executions) != 0 || p.Failed() {
		t.Errorf("executions=%+v failed=%v", r.executions, p.Failed())
	}
}

func TestExamplesAndFuzzSeedsAreExecutions(t *testing.T) {
	_, r := parse(t,
		ev(0, "run", "FuzzD", ""),
		ev(0, "run", "FuzzD/seed#0", ""),
		ev(0, "pass", "FuzzD/seed#0", "", `"Elapsed":0`),
		ev(0, "pass", "FuzzD", "", `"Elapsed":0`),
		ev(0, "run", "ExampleHello", ""),
		ev(0, "output", "ExampleHello", "x\n"),
		ev(0, "pass", "ExampleHello", "", `"Elapsed":0`),
		ev(0, "pass", "", "", `"Elapsed":1`),
	)
	if len(r.executions) != 3 {
		t.Errorf("executions = %+v", r.executions)
	}
}

// A panicking test makes the library export from the test's cleanup, so
// the line arrives attributed to the test, possibly more than once; it is
// reported once and kept out of the failure detail.
func TestLibraryExportLineOnATestIsReportedOnceAndNotAsOutput(t *testing.T) {
	_, r := parse(t,
		ev(0, "run", "TestPanics", ""),
		ev(0, "output", "TestPanics", "    pn_test.go:4: before\n"),
		ev(0, "output", "TestPanics", "buildkite-test-collector: exported 1 test executions from example.com/t2j/p\n"),
		ev(0, "output", "TestPanics", "buildkite-test-collector: exported 1 test executions from example.com/t2j/p\n"),
		ev(0, "fail", "TestPanics", "", `"Elapsed":0`),
		ev(0, "fail", "", "", `"Elapsed":0`),
	)
	if strings.Join(r.exported, ",") != pkg {
		t.Errorf("exported = %v", r.exported)
	}
	if got := r.byName(t, "TestPanics").Result.Exception.Message; got != "pn_test.go:4: before" {
		t.Errorf("message = %q", got)
	}
}

func TestLinesThatAreNotEventsAreReportedNotParsed(t *testing.T) {
	p, r := parse(t,
		"=== RUN   TestPass",
		"--- PASS: TestPass (0.00s)",
		"",
		`{"not":"an event"}`,
		`{"Action":"deploy","Package":"api"}`,
		"ok  \texample.com/t2j/p\t0.1s",
	)
	if p.Events() != 0 || r.nonEvents != 6 || len(r.executions) != 0 {
		t.Errorf("events=%d nonEvents=%d executions=%v", p.Events(), r.nonEvents, r.executions)
	}
}
