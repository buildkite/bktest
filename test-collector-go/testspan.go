package bktest

import (
	"context"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

type executionKey struct{}

// testExecution is one running test: its root span on the private provider
// and its test.body phase span on the provider carrying the forwarder.
type testExecution struct {
	c        *collector
	span     trace.Span
	body     trace.Span
	traceID  trace.TraceID
	start    time.Time
	identity testIdentity
	external string
	ctx      context.Context

	mu   sync.Mutex
	tags map[string]string
}

func executionFromContext(ctx context.Context) *testExecution {
	if ctx == nil {
		return nil
	}
	ex, _ := ctx.Value(executionKey{}).(*testExecution)
	return ex
}

var (
	executionsMu sync.Mutex
	executions   = map[testing.TB]*testExecution{}
)

// startTest starts the spans for t and schedules their finish in t.Cleanup.
// A failure to start leaves the test running without a span and counts as a
// dropped result. The executions lock is held throughout so a test is never
// started twice and is published only once fully built.
func (c *collector) startTest(t testing.TB) (ctx context.Context) {
	executionsMu.Lock()
	defer executionsMu.Unlock()
	if ex, ok := executions[t]; ok {
		return ex.ctx
	}

	defer func() {
		if r := recover(); r != nil {
			c.testStats.startFailure()
			warnf("TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (could not start span: %v).\n%s", r, debug.Stack())
			warnf("OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.")
			ctx = context.Background()
		}
	}()

	start := now()
	identity := identify(t)
	ex := &testExecution{c: c, start: start, identity: identity, external: uuid.Must(uuid.NewV7()).String(), tags: map[string]string{}}

	startAttrs := []attribute.KeyValue{
		attribute.String(attrExecutionVia, executionVia),
		attribute.String(attrRunKey, c.env.key),
		semconv.TestCaseResultStatusKey.String(resultUnset),
	}
	startAttrs = append(startAttrs, ex.testAttributes()...)

	rootCtx, span := c.testTracer.Start(context.Background(), testSpanName,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(start),
		trace.WithLinks(c.links...),
		trace.WithAttributes(startAttrs...),
	)
	ex.span = span
	ex.traceID = span.SpanContext().TraceID()
	ex.ctx = context.WithValue(rootCtx, executionKey{}, ex)

	t.Cleanup(func() {
		end := now()
		executionsMu.Lock()
		delete(executions, t)
		executionsMu.Unlock()
		ex.finish(t, end, inPanic())
	})

	// The body phase lives on the suite's provider, which the collector does
	// not control; if it cannot start one the test still has its result.
	attempt("Could not start the OpenTelemetry test.body phase span", func() {
		ex.ctx, ex.body = c.phaseTracer().Start(ex.ctx, bodyPhaseName, trace.WithSpanKind(trace.SpanKindInternal))
	})
	executions[t] = ex
	return ex.ctx
}

// phaseTracer comes from the provider that carries the forwarder, so phase
// and instrumentation spans share one pipeline.
func (c *collector) phaseTracer() trace.Tracer {
	tp := c.childProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(tracerName, trace.WithInstrumentationVersion(Version))
}

// finish maps the outcome of t onto the body and test spans and ends them
// with the end time captured when the test returned. Every step recovers on
// its own: a half-described execution beats a missing one.
func (ex *testExecution) finish(t testing.TB, end time.Time, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			warnf("Could not finish the OpenTelemetry test span: %v\n%s", r, debug.Stack())
		}
	}()

	// go test runs Cleanup before it marks a panicking test as failed, so
	// t.Failed is not enough on its own.
	status := resultPass
	switch {
	case t.Failed() || panicked:
		status = resultFail
	case t.Skipped():
		status = resultSkipped
	}
	reason := ""
	if status == resultFail {
		reason = "test failed"
		if panicked {
			reason = "test panicked"
		}
	}
	recordFailure := func(span trace.Span) {
		if status != resultFail {
			return
		}
		span.SetStatus(codes.Error, reason)
		if panicked {
			span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
				semconv.ExceptionType(exceptionTypePanic),
				semconv.ExceptionMessage("test panicked; go test prints the panic value after the test output"),
				semconv.ExceptionStacktrace(truncate(string(debug.Stack()), testSpanEventAttributeLength)),
			))
		}
	}

	if ex.body != nil {
		attempt("Could not finish OpenTelemetry phase span", func() {
			recordFailure(ex.body)
			ex.endSpan(ex.body, end)
		})
	}
	attempt("Could not record the OpenTelemetry test result", func() {
		ex.span.SetAttributes(semconv.TestCaseResultStatusKey.String(status))
		recordFailure(ex.span)
	})
	attempt("Could not describe OpenTelemetry test span", func() {
		ex.span.SetAttributes(ex.testAttributes()...)
		ex.span.SetAttributes(ex.c.runAttrs...)
		ex.span.SetAttributes(tagAttributes(ex.mergedTags())...)
	})
	attempt("Could not finish OpenTelemetry test span", func() {
		ex.endSpan(ex.span, end)
	})

	if panicked {
		// go test exits as soon as the panic resurfaces; Main never regains
		// control, so this is the last chance to deliver the result.
		ex.c.abort()
	}
}

// endSpan applies the captured end time unless the clock stepped backwards,
// in which case the SDK's own clock wins.
func (ex *testExecution) endSpan(span trace.Span, end time.Time) {
	if end.Before(ex.start) {
		span.End()
		return
	}
	span.End(trace.WithTimestamp(end))
}

func attempt(failure string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			warnf("%s: %v", failure, r)
		}
	}()
	fn()
}

func (ex *testExecution) tag(key, value string) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	ex.tags[key] = strings.ToValidUTF8(value, "\uFFFD")
}

// mergedTags returns run-level tags with per-test tags overriding them.
func (ex *testExecution) mergedTags() map[string]string {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	merged := make(map[string]string, len(ex.c.tags)+len(ex.tags))
	maps.Copy(merged, ex.c.tags)
	maps.Copy(merged, ex.tags)
	return merged
}

func (ex *testExecution) annotate(content string) {
	// The SDK's attribute length limit does not apply to event attributes.
	ex.span.AddEvent(annotationEventName, trace.WithAttributes(attribute.String(attrAnnotation, truncate(content, testSpanEventAttributeLength))))
}

// testAttributes are the identity attributes, without tags.
func (ex *testExecution) testAttributes() []attribute.KeyValue {
	id := ex.identity
	attrs := []attribute.KeyValue{
		attribute.String(attrTestScope, id.scope),
		attribute.String(attrTestName, id.name),
		semconv.TestCaseName(id.scope + "." + id.name),
		semconv.TestSuiteName(id.scope),
	}
	if id.file != "" {
		attrs = append(attrs, semconv.CodeFilePath(ex.c.locationPrefix+id.file))
		if id.line > 0 {
			attrs = append(attrs, semconv.CodeLineNumber(id.line))
		}
	}
	return append(attrs, attribute.String(attrExternalID, ex.external))
}

// testIdentity is what Test Engine uses to recognise a test across runs:
// scope is the package import path, name is t.Name(), and the location is
// the file and first line of the function that called Start, relative to the
// module root.
type testIdentity struct {
	scope string
	name  string
	file  string
	line  int
}

var collectorPackagePath = reflect.TypeOf(collector{}).PkgPath()

// identify finds the first caller outside this package and derives the
// test's identity from that function.
func identify(t testing.TB) testIdentity {
	id := testIdentity{name: strings.ToValidUTF8(t.Name(), "\uFFFD")}
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function != "" && !strings.HasPrefix(frame.Function, collectorPackagePath+".") {
			id.scope = packagePath(frame.Function)
			id.file, id.line = frame.File, frame.Line
			if fn := runtime.FuncForPC(frame.Entry); fn != nil {
				id.file, id.line = fn.FileLine(fn.Entry())
			}
			id.file = strings.ToValidUTF8(moduleRelative(id.file), "\uFFFD")
			break
		}
		if !more {
			break
		}
	}
	if id.scope == "" {
		id.scope = "unknown"
	}
	return id
}

// packagePath extracts the import path from a runtime function name such
// as "example.com/m/pkg_test.TestX.func1", dropping the _test suffix of an
// external test package so both halves of a package share one scope. The
// linker escapes dots and other characters in the last path element as %xx
// ("gopkg.in/yaml%2ev3"), which is undone here.
func packagePath(function string) string {
	pkg := function
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		if j := strings.Index(pkg[i:], "."); j >= 0 {
			pkg = pkg[:i+j]
		}
	} else if j := strings.Index(pkg, "."); j >= 0 {
		pkg = pkg[:j]
	}
	pkg = strings.TrimSuffix(pkg, "_test")
	if unescaped, err := url.PathUnescape(pkg); err == nil {
		pkg = unescaped
	}
	return pkg
}

// moduleRelative strips the module root from a source path, so the location
// is stable across machines: the directory containing go.mod for absolute
// paths, the module path for the import-path-shaped names -trimpath records.
func moduleRelative(file string) string {
	if !filepath.IsAbs(file) {
		file = filepath.ToSlash(file)
		if module := mainModulePath(); module != "" {
			return strings.TrimPrefix(file, module+"/")
		}
		return file
	}
	root := findModuleRoot(filepath.Dir(file))
	if root == "" {
		return filepath.ToSlash(file)
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return filepath.ToSlash(file)
	}
	return filepath.ToSlash(rel)
}

// mainModulePath is the module under test: from the build info when the
// toolchain fills it in for test binaries (Go 1.24+), otherwise from the
// go.mod above the working directory, which go test sets to the package's
// source directory.
var mainModulePath = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Path != "" {
		return info.Main.Path
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root := findModuleRoot(cwd)
	if root == "" {
		return ""
	}
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(gomod), "\n") {
		if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(module), `"`)
		}
	}
	return ""
})

func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// inPanic reports whether the caller runs while a panic unwinds the
// goroutine, which is how go test runs Cleanup functions for a test that
// panicked.
func inPanic() bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function == "runtime.gopanic" {
			return true
		}
		if !more {
			return false
		}
	}
}

// truncate limits s to limit runes, marking the cut with "..."; the SDK
// applies no length limit to event attribute values.
func truncate(s string, limit int) string {
	if limit <= 3 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-3]) + "..."
}
