// Package bktest reports go test results to Buildkite Test Engine over OTLP
// from inside the test binary, with child spans for the test's own
// instrumentation. Most suites do not need it: pipe go test -json through
// the buildkite-test-collector command instead, which reports every test
// without touching the source. Use this package when you want child spans,
// tags or annotations.
//
// Wire it up once per package in TestMain and once per test:
//
//	func TestMain(m *testing.M) {
//		os.Exit(bktest.Main(m))
//	}
//
//	func TestAdd(t *testing.T) {
//		ctx := bktest.Start(t)
//		result := compute(ctx) // ctx carries the test span for instrumentation
//		...
//	}
//
// Only tests that call Start are reported. Export needs a token
// (BUILDKITE_TESTS_OTLP_TOKEN or BUILDKITE_ANALYTICS_TOKEN) and a valid run
// key. Without a token every function is a silent no-op, so go test on a
// developer's machine runs and prints exactly as it would without this
// package; with a token but an invalid run key or endpoint the first Start
// prints one warning and the tests run the same way.
//
// Do not combine this package with the buildkite-test-collector command on
// the same package: every test would be reported twice.
package bktest

import (
	"context"
	"fmt"
	"maps"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Version is the collector version reported on every test execution.
const Version = collector.Version

type options = collector.Options

// Option configures Main.
type Option func(*options)

// SpanFilter decides whether a finished child span is exported to Buildkite.
// It never sees test.execution or phase spans.
type SpanFilter func(span sdktrace.ReadOnlySpan) bool

// WithTags adds run-level tags to every test execution as buildkite.tag.<key>.
func WithTags(tags map[string]string) Option {
	return func(o *options) {
		if o.Tags == nil {
			o.Tags = map[string]string{}
		}
		maps.Copy(o.Tags, tags)
	}
}

// WithLocationPrefix prepends prefix to every test's file path, for suites
// that run from a subdirectory of the repository.
func WithLocationPrefix(prefix string) Option {
	return func(o *options) { o.LocationPrefix = prefix }
}

// WithSpanFilter drops child spans the filter rejects before they are sent to
// Buildkite. Test spans are never filtered.
func WithSpanFilter(filter SpanFilter) Option {
	return func(o *options) { o.Filter = filter }
}

// WithTracerProvider names the suite's own tracer provider. The collector
// attaches its child span forwarder to it and leaves its sampler, resource
// and processors alone. Without this option the collector attaches to the
// global provider as Main found it when that is an SDK provider, installs
// its own when nobody has configured one, and otherwise exports test spans
// only. A provider a test installs later is never replaced or attached to.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.TracerProvider = tp }
}

// newOptions starts from the environment and lets explicit options override it.
func newOptions(opts []Option) options {
	o := options{LocationPrefix: os.Getenv("BUILDKITE_ANALYTICS_LOCATION_PREFIX"), ChildSpans: true, QuietWithoutToken: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

var (
	mainCalled  atomic.Bool
	mainOptions []Option
	// globalProvider is the global tracer provider as Main found it, so
	// the collector set up from inside the first test attaches to the
	// provider the suite configured and not to one that test installed.
	globalProvider trace.TracerProvider
	// current is the collector configured for this process, nil until the
	// first Start, Tag or Annotate and when export is disabled.
	current   atomic.Pointer[collector.Collector]
	setupMu   sync.Mutex
	setupDone bool
)

// Main runs the package's tests through m.Run, exports their results and
// returns m.Run's exit code unchanged. Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(bktest.Main(m)) }
//
// Without a token Main runs the tests silently. Other setup failures and
// export failures are reported on stderr and never change the exit code.
// After export Main prints one line on stderr saying how many executions it
// exported for the package, which the buildkite-test-collector command uses
// to detect a suite that is reported twice.
func Main(m *testing.M, opts ...Option) int {
	mainCalled.Store(true)
	mainOptions = opts
	globalProvider = otel.GetTracerProvider()
	testPackage = callerPackage()

	code := m.Run()

	if c := current.Load(); c != nil {
		finishAllPending()
		c.Shutdown()
		fmt.Fprintln(os.Stderr, collector.ExportedLine(c.Exported(), testPackage))
	}
	return code
}

// configured returns the collector for this process, setting it up on
// first use. Setup waits for the first test rather than running in Main
// because go test only records the environment variables a test binary
// reads while m.Run is running, and caches a package's result keyed on
// those reads: configured inside a test, the package re-runs when the
// token, run key or endpoint change, and is served from the cache only
// when they are the same.
func configured() *collector.Collector {
	if c := current.Load(); c != nil {
		return c
	}
	if !mainCalled.Load() {
		return nil
	}
	setupMu.Lock()
	defer setupMu.Unlock()
	if !setupDone {
		setupDone = true
		o := newOptions(mainOptions)
		o.GlobalTracerProvider = globalProvider
		current.Store(collector.Setup(context.Background(), o))
	}
	return current.Load()
}

// callerPackage is the import path of the package that called Main: the
// test binary's build info names it on Go 1.24 and later, and before that
// the frame that called Main does.
func callerPackage() string {
	if path := mainPackagePath(); path != "" {
		return path
	}
	pcs := make([]uintptr, 8)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function != "" {
			return importPath(frame.Function)
		}
		if !more {
			return "unknown"
		}
	}
}

// Start records t as a test execution and returns the context its
// instrumentation should use. The execution ends in t.Cleanup; its result
// is read from t.Failed and t.Skipped once go test has finished the test,
// so a cleanup registered before Start that fails the test is reflected.
// A panicking test is reported as failed. Calling Start twice for the same
// test returns the same context. Call Start after t.Parallel(): the span
// starts at Start, so the wait for the parent would count as duration.
//
// Without Main, or when export is disabled, Start returns a plain context.
func Start(t testing.TB) context.Context {
	c := configured()
	if c == nil {
		if !mainCalled.Load() {
			collector.WarnOnce("start-without-main", "bktest.Start was called but bktest.Main did not run; call os.Exit(bktest.Main(m)) from TestMain so test results are exported.")
		}
		return context.Background()
	}
	return startTest(c, t)
}

// Tag adds a per-test tag that becomes buildkite.tag.<key> on the execution.
// It overrides a run-level tag with the same key. When export is disabled it
// does nothing; for a test that has not called Start it drops the tag and
// warns once.
func Tag(t testing.TB, key, value string) {
	ex := lookup(t)
	if ex == nil {
		if configured() == nil {
			return
		}
		collector.WarnOnce("tag-without-start", "bktest.Tag was called for %s before bktest.Start; the tag %q was ignored. Further calls will not be reported.", t.Name(), key)
		return
	}
	ex.Tag(key, value)
}

// Annotate adds a test.annotation event to the test execution. When export
// is disabled it does nothing; for a test that has not called Start it drops
// the annotation and warns once.
func Annotate(t testing.TB, content string) {
	ex := lookup(t)
	if ex == nil {
		if configured() == nil {
			return
		}
		collector.WarnOnce("annotate-without-start", "bktest.Annotate was called for %s before bktest.Start; the annotation was ignored. Further calls will not be reported.", t.Name())
		return
	}
	ex.Annotate(content)
}
