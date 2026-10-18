package collector

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestMain silences the SDK's own error handler: the tests below provoke
// export failures and global provider resets on purpose and assert on the
// collector's warnings instead.
func TestMain(m *testing.M) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	os.Exit(m.Run())
}

const (
	fixtureScope = "example.com/m/pkg"
	fixtureFile  = "pkg/pkg_test.go"
)

func captureWarnings(t *testing.T) *otlptest.Buffer {
	t.Helper()
	buf := &otlptest.Buffer{}
	t.Cleanup(RedirectWarnings(buf))
	return buf
}

func withTags(tags map[string]string) func(*Options) {
	return func(o *Options) { o.Tags = tags }
}

func withLocationPrefix(prefix string) func(*Options) {
	return func(o *Options) { o.LocationPrefix = prefix }
}

func withFilter(filter func(sdktrace.ReadOnlySpan) bool) func(*Options) {
	return func(o *Options) { o.Filter = filter }
}

func withTracerProvider(tp trace.TracerProvider) func(*Options) {
	return func(o *Options) { o.TracerProvider = tp }
}

// configured runs New against the receiver with a valid token and run key
// and child spans enabled, failing the test if setup does not succeed, and
// restores the global tracer provider afterwards.
func configured(t *testing.T, r *otlptest.Receiver, env map[string]string, opts ...func(*Options)) *Collector {
	t.Helper()
	vars := map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "secret-token",
		"BUILDKITE_ANALYTICS_KEY":           "run-key-" + strings.ReplaceAll(t.Name(), "/", "_"),
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.Endpoint(),
	}
	for k, v := range env {
		vars[k] = v
	}
	otlptest.ClearEnv(t, vars)
	restoreGlobalProvider(t)

	o := Options{ChildSpans: true}
	for _, opt := range opts {
		opt(&o)
	}
	c, err := New(context.Background(), o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Shutdown)
	return c
}

func restoreGlobalProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
}

var (
	passed  = Result{Status: StatusPass}
	failed  = Result{Status: StatusFail, Reason: "test failed"}
	skipped = Result{Status: StatusSkipped}
)

// run drives one execution from Start to Finish. body, when set, runs
// between the two with the execution and decides the result.
func run(c *Collector, name string, body func(ex *Execution) Result) {
	start := time.Now()
	ex := c.Start(Identity{Scope: fixtureScope, Name: name, File: fixtureFile, Line: 10}, start)
	if ex == nil {
		return
	}
	r := passed
	if body != nil {
		r = body(ex)
	}
	ex.Finish(time.Now(), r)
}

// fails is a run body that fails the test after fn.
func fails(fn func(ex *Execution)) func(ex *Execution) Result {
	return func(ex *Execution) Result {
		fn(ex)
		return failed
	}
}

// passes is a run body that passes the test after fn.
func passes(fn func(ex *Execution)) func(ex *Execution) Result {
	return func(ex *Execution) Result {
		fn(ex)
		return passed
	}
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }
