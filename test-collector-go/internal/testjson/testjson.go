// Package testjson turns the event stream of go test -json into test
// executions. It keeps the per-test output needed for a failure report and
// nothing else, so it can run alongside a long suite.
package testjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
)

// Event is one line of go test -json output, as cmd/test2json defines it.
// FailedBuild is set on a package's fail event when its build failed (Go
// 1.24+). Key and Value are a t.Attr pair on an attr event (Go 1.25+).
// OutputType marks the framing lines go test prints around a test's own
// output (Go 1.27+).
type Event struct {
	Time        time.Time
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	OutputType  string
	FailedBuild string
	Key         string
	Value       string
}

// actions are the event kinds test2json emits; any other Action field
// belongs to some other JSON stream.
var actions = map[string]bool{
	"start": true, "run": true, "pause": true, "cont": true, "output": true,
	"bench": true, "pass": true, "fail": true, "skip": true,
	"build-output": true, "build-fail": true, "attr": true,
}

// Execution is one test's run as reconstructed from the stream.
type Execution struct {
	Package string
	// Test is the full test name as t.Name() reports it, including the
	// parent's name for a subtest.
	Test   string
	Start  time.Time
	End    time.Time
	Result collector.Result
	// Tags are the test's t.Attr pairs; a repeated key keeps its last
	// value.
	Tags map[string]string
}

// Handler receives what the parser finds. Every field must be set; the
// parser calls them without nil checks.
type Handler struct {
	// Execution receives each finished test as its result arrives. Tests
	// still running when their package or the stream ends are failed then,
	// in no particular order.
	Execution func(Execution)
	// PackageStarted is called once per package as soon as the stream
	// mentions it, so a caller can start looking up test locations early.
	PackageStarted func(pkg string)
	// PackageFailedBeforeTests is called for a package that failed without
	// running a single test: a build failure, or TestMain exiting early.
	PackageFailedBeforeTests func(pkg string)
	// LibraryExported is called when the package's own TestMain reported
	// that the bktest library exported its results, so the caller can warn
	// about the duplication.
	LibraryExported func(pkg string)
}

// Parser consumes events one line at a time.
type Parser struct {
	h        Handler
	packages map[string]*packageState
	events   int
	failed   bool
	// labelledFrames is set once the stream has labelled a framing line
	// (Go 1.27+); from then on only labelled lines are framing.
	labelledFrames bool
}

type packageState struct {
	running         map[string]*testState
	ran             bool
	libraryExported bool
}

type testState struct {
	start  time.Time
	output []string
	attrs  map[string]string
}

// NewParser returns a parser reporting to h.
func NewParser(h Handler) *Parser {
	return &Parser{h: h, packages: map[string]*packageState{}}
}

// Events is the number of events parsed so far.
func (p *Parser) Events() int { return p.events }

// Failed reports whether any test or package failed.
func (p *Parser) Failed() bool { return p.failed }

// Line parses one line of input. For an event it returns the event's Output
// text, which the caller should write through so the stream reads like
// go test -v; for anything else it returns false.
func (p *Parser) Line(line []byte) (output string, ok bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return "", false
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil || !actions[ev.Action] {
		return "", false
	}
	p.events++
	p.handle(ev)
	return ev.Output, true
}

func (p *Parser) handle(ev Event) {
	// build-output and build-fail events carry no Package; the package's
	// own fail event reports the failed build.
	pkgName := ev.Package
	if pkgName == "" {
		return
	}
	pkg := p.packages[pkgName]
	if pkg == nil {
		pkg = &packageState{running: map[string]*testState{}}
		p.packages[pkgName] = pkg
		p.h.PackageStarted(pkgName)
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}

	if ev.Action == "output" {
		// bktest.Main prints this line after exporting the package. It is
		// usually package output, but arrives as the test's output when a
		// panicking test's cleanup forced the export, so it is matched
		// before the event is routed by Test.
		if exported, ok := collector.ExportedPackage(ev.Output); ok {
			if !pkg.libraryExported {
				pkg.libraryExported = true
				p.h.LibraryExported(exported)
			}
			return
		}
	}
	if ev.Test == "" {
		p.handlePackageEvent(pkgName, pkg, ev)
		return
	}
	if isBenchmark(ev.Test) {
		// Benchmarks get run events but no terminal event when they
		// succeed; a failing one still fails its package.
		return
	}
	switch ev.Action {
	case "run":
		pkg.ran = true
		pkg.running[ev.Test] = &testState{start: ev.Time}
	case "output":
		if ts := pkg.running[ev.Test]; ts != nil && !p.isFraming(ev) {
			ts.output = append(ts.output, ev.Output)
		}
	case "attr":
		if ts := pkg.running[ev.Test]; ts != nil && ev.Key != "" {
			if ts.attrs == nil {
				ts.attrs = map[string]string{}
			}
			ts.attrs[ev.Key] = ev.Value
		}
	case "pass", "fail", "skip":
		ts := pkg.running[ev.Test]
		if ts == nil {
			return
		}
		delete(pkg.running, ev.Test)
		if ev.Action == "fail" {
			p.failed = true
		}
		p.emit(pkgName, ev.Test, ts, testEnd(ts.start, ev), ev.Action)
	}
}

func isBenchmark(test string) bool {
	return strings.HasPrefix(test, "Benchmark")
}

// elapsedResolution is the precision of the Elapsed field: go test prints
// it with two decimals, so any test under 5 ms reports 0.
const elapsedResolution = 10 * time.Millisecond

// testEnd derives the end of a test from its terminal event. Elapsed is the
// test's own measurement, so it wins whenever it is non-zero; otherwise the
// event timestamps bound the duration, which is at most one Elapsed tick.
func testEnd(start time.Time, ev Event) time.Time {
	if ev.Elapsed > 0 {
		return start.Add(time.Duration(ev.Elapsed * float64(time.Second)))
	}
	end := ev.Time
	if limit := start.Add(elapsedResolution); end.After(limit) {
		end = limit
	}
	if end.Before(start) {
		return start
	}
	return end
}

func (p *Parser) handlePackageEvent(pkgName string, pkg *packageState, ev Event) {
	switch ev.Action {
	case "fail":
		p.failed = true
		if ev.FailedBuild != "" || !pkg.ran {
			p.h.PackageFailedBeforeTests(pkgName)
		}
		p.finishRunning(pkgName, pkg, ev.Time)
	case "pass", "skip":
		p.finishRunning(pkgName, pkg, ev.Time)
	}
}

// finishRunning fails every test that is still running when its package
// ends: a panic or a timeout killed the binary before they could report.
func (p *Parser) finishRunning(pkgName string, pkg *packageState, end time.Time) {
	for name, ts := range pkg.running {
		delete(pkg.running, name)
		p.failed = true
		p.emit(pkgName, name, ts, end, "fail")
	}
}

// Finish ends the stream: tests still running in a package that never
// ended are failed, since go test itself must have died.
func (p *Parser) Finish() {
	now := time.Now()
	for pkgName, pkg := range p.packages {
		p.finishRunning(pkgName, pkg, now)
	}
}

func (p *Parser) emit(pkgName, test string, ts *testState, end time.Time, action string) {
	ex := Execution{Package: pkgName, Test: test, Start: ts.start, End: end, Tags: ts.attrs}
	switch action {
	case "pass":
		ex.Result = collector.Result{Status: collector.StatusPass}
	case "skip":
		ex.Result = collector.Result{Status: collector.StatusSkipped}
	default:
		ex.Result = failure(ts.output)
	}
	p.h.Execution(ex)
}

// failure builds the failed result from the test's own output: the panic
// line as the reason and the trace as the stack when it panicked, otherwise
// the first line as the reason; everything before any trace is the detail.
func failure(output []string) collector.Result {
	text := dedent(output)
	r := collector.Result{Status: collector.StatusFail, Reason: "test failed"}
	if text == "" {
		return r
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "panic: ") {
			r.Reason = line
			r.Exception = &collector.Exception{Type: "panic", Message: strings.Join(lines[:i+1], "\n"), Stacktrace: strings.Join(lines[i+1:], "\n")}
			return r
		}
	}
	for _, line := range lines {
		if l := strings.TrimSpace(line); l != "" {
			r.Reason = l
			break
		}
	}
	r.Exception = &collector.Exception{Type: "failure", Message: text}
	return r
}

// dedent removes the four spaces go test puts in front of a test's log
// lines (continuation lines keep their extra indentation) and trims
// trailing blank lines.
func dedent(output []string) string {
	lines := strings.Split(strings.Join(output, ""), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "    ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// isFraming reports the lines go test prints around a test's own output.
// Go 1.27 labels them; before that only their prefix tells them apart.
func (p *Parser) isFraming(ev Event) bool {
	if ev.OutputType == "frame" {
		p.labelledFrames = true
	}
	if p.labelledFrames {
		return ev.OutputType == "frame"
	}
	output := strings.TrimLeft(ev.Output, " ")
	for _, prefix := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "=== ATTR", "--- PASS", "--- FAIL", "--- SKIP", "--- BENCH"} {
		if strings.HasPrefix(output, prefix) {
			return true
		}
	}
	return false
}
