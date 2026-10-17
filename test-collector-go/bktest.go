// Package bktest reports go test results to Buildkite Test Engine over OTLP.
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
// Export needs a token (BUILDKITE_TESTS_OTLP_TOKEN or BUILDKITE_ANALYTICS_TOKEN)
// and a valid run key. Without them Main prints one warning and every other
// function is a no-op, so go test runs exactly as it would without this
// package.
package bktest

import (
	"context"
	"runtime/debug"
	"sync/atomic"
	"testing"
)

// Version is the collector version reported on every test execution.
const Version = "0.1.0"

var mainCalled atomic.Bool

// Main runs the package's tests through m.Run, exports their results and
// returns m.Run's exit code unchanged. Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(bktest.Main(m)) }
//
// Setup failures and export failures are reported on stderr and never change
// the exit code.
func Main(m *testing.M, opts ...Option) int {
	mainCalled.Store(true)
	c := setUp(newOptions(opts))
	current.Store(c)

	code := m.Run()

	if c != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					warnf("Could not shut down OpenTelemetry span export: %v\n%s", r, debug.Stack())
				}
			}()
			c.shutdown()
		}()
	}
	return code
}

// setUp configures export, turning any failure into a warning and a nil
// collector so a misconfigured collector never stops the tests.
func setUp(o options) (c *collector) {
	defer func() {
		if r := recover(); r != nil {
			warnf("OpenTelemetry span export disabled: %v\n%s", r, debug.Stack())
			c = nil
		}
	}()
	c, err := configure(context.Background(), o)
	if err != nil {
		warnf("OpenTelemetry span export disabled: %v", err)
		return nil
	}
	return c
}

// Start records t as a test execution and returns the context its
// instrumentation should use. The execution finishes in t.Cleanup with the
// result read from t.Failed and t.Skipped; a panicking test is reported as
// failed. Calling Start twice for the same test returns the same context.
//
// Without Main, or when export is disabled, Start returns a plain context.
func Start(t testing.TB) context.Context {
	c := current.Load()
	if c == nil {
		if !mainCalled.Load() {
			warnOnce("start-without-main", "bktest.Start was called but bktest.Main did not run; call os.Exit(bktest.Main(m)) from TestMain so test results are exported.")
		}
		return context.Background()
	}
	return c.startTest(t)
}

// Tag adds a per-test tag that becomes buildkite.tag.<key> on the execution.
// It overrides a run-level tag with the same key. When export is disabled it
// does nothing; with a context from outside a test it drops the tag and warns
// once.
func Tag(ctx context.Context, key, value string) {
	ex := executionFromContext(ctx)
	if ex == nil {
		if current.Load() == nil {
			return
		}
		warnOnce("tag-outside-test", "bktest.Tag was called with a context that does not belong to a running test; the tag %q was ignored. Further calls will not be reported.", key)
		return
	}
	ex.tag(key, value)
}

// Annotate adds a test.annotation event to the test execution. When export
// is disabled it does nothing; with a context from outside a test it drops
// the annotation and warns once.
func Annotate(ctx context.Context, content string) {
	ex := executionFromContext(ctx)
	if ex == nil {
		if current.Load() == nil {
			return
		}
		warnOnce("annotate-outside-test", "bktest.Annotate was called with a context that does not belong to a running test; the annotation was ignored. Further calls will not be reported.")
		return
	}
	ex.annotate(content)
}
