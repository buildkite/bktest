// Package testjson turns the event stream of go test -json into test
// executions. It keeps the per-test output needed for a failure report and
// nothing else, so it can run alongside a long suite.
package testjson

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
)

// Event is one line of go test -json output, as cmd/test2json defines it.
// FailedBuild is set on a package's fail event when its build failed (Go
// 1.24+).
type Event struct {
	Time        time.Time
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	FailedBuild string
}

// ErrNotEvent reports a line that is not a go test -json event.
var ErrNotEvent = errors.New("not a go test -json event")

// Execution is one test's run as reconstructed from the stream.
type Execution struct {
	Package string
	// Test is the full test name as t.Name() reports it, including the
	// parent's name for a subtest.
	Test   string
	Start  time.Time
	End    time.Time
	Result collector.Result
}

// Handler receives what the parser finds. Every field is optional.
type Handler struct {
	// Execution receives each finished test, in the order the stream
	// reports their results.
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
}

type packageState struct {
	running map[string]*testState
	ran     bool
}

type testState struct {
	start  time.Time
	output []string
}

// NewParser returns a parser reporting to h.
func NewParser(h Handler) *Parser {
	return &Parser{h: h, packages: map[string]*packageState{}}
}

// Events is the number of events parsed so far.
func (p *Parser) Events() int { return p.events }

// Failed reports whether any test or package failed.
func (p *Parser) Failed() bool { return p.failed }

// Line parses one line of input. It returns ErrNotEvent for a line that is
// not an event, and the event's Output text, which the caller should write
// through so the stream reads like go test -v.
func (p *Parser) Line(line []byte) (output string, err error) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return "", ErrNotEvent
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil || ev.Action == "" {
		return "", ErrNotEvent
	}
	p.events++
	p.handle(ev)
	return ev.Output, nil
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
		if p.h.PackageStarted != nil {
			p.h.PackageStarted(pkgName)
		}
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}

	if ev.Test == "" {
		p.handlePackageEvent(pkgName, pkg, ev)
		return
	}
	switch ev.Action {
	case "run":
		pkg.ran = true
		pkg.running[ev.Test] = &testState{start: ev.Time}
	case "cont":
		// A paused parallel test resumes here; its Elapsed counts from now.
		if ts := pkg.running[ev.Test]; ts != nil {
			ts.start = ev.Time
		}
	case "output":
		if ts := pkg.running[ev.Test]; ts != nil && !isFraming(ev.Output) {
			ts.output = append(ts.output, ev.Output)
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
		p.emit(pkgName, ev.Test, ts, ts.start.Add(time.Duration(ev.Elapsed*float64(time.Second))), ev.Action)
	}
}

func (p *Parser) handlePackageEvent(pkgName string, pkg *packageState, ev Event) {
	switch ev.Action {
	case "output":
		if pkg, ok := collector.ExportedPackage(ev.Output); ok && p.h.LibraryExported != nil {
			p.h.LibraryExported(pkg)
		}
	case "fail":
		p.failed = true
		if ev.FailedBuild != "" || !pkg.ran {
			if p.h.PackageFailedBeforeTests != nil {
				p.h.PackageFailedBeforeTests(pkgName)
			}
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
	if p.h.Execution == nil {
		return
	}
	ex := Execution{Package: pkgName, Test: test, Start: ts.start, End: end}
	switch action {
	case "pass":
		ex.Result = collector.Result{Status: collector.StatusPass}
	case "skip":
		ex.Result = collector.Result{Status: collector.StatusSkipped}
	default:
		ex.Result = failure(test, ts.output)
	}
	p.h.Execution(ex)
}

// failure builds the failed result from the test's own output: the panic
// line and trace when it panicked, otherwise the first line as the reason
// and everything as the detail.
func failure(test string, output []string) collector.Result {
	text := dedent(output, strings.Count(test, "/")+1)
	r := collector.Result{Status: collector.StatusFail, Reason: "test failed"}
	if text == "" {
		return r
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "panic: ") {
			r.Reason = line
			r.Exception = &collector.Exception{Type: "panic", Message: line, Stacktrace: strings.Join(lines[i+1:], "\n")}
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

// dedent removes the indentation go test adds for a test at the given
// nesting depth, keeping any further indentation of continuation lines, and
// trims trailing blank lines.
func dedent(output []string, depth int) string {
	indent := strings.Repeat(" ", 4*depth)
	var b strings.Builder
	for _, line := range output {
		b.WriteString(strings.TrimPrefix(line, indent))
	}
	return strings.TrimRight(b.String(), "\n")
}

// isFraming reports the lines go test prints around a test's own output.
func isFraming(line string) bool {
	for _, prefix := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- PASS", "--- FAIL", "--- SKIP", "--- BENCH"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
