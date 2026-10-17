package bktest

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestMain silences the SDK's own error handler: the tests below provoke
// export failures and global provider resets on purpose and assert on the
// collector's warnings instead.
func TestMain(m *testing.M) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	os.Exit(m.Run())
}

// receivedRequest is one OTLP export request as the loopback receiver saw it.
type receivedRequest struct {
	Header http.Header
	Body   *coltracepb.ExportTraceServiceRequest
}

func (r receivedRequest) spans() []*tracepb.Span {
	var out []*tracepb.Span
	for _, rs := range r.Body.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			out = append(out, ss.Spans...)
		}
	}
	return out
}

// receiver is a loopback stand-in for tests-otlp.buildkite.com: it gunzips
// and decodes every request, records it, and answers with the configured
// status, or 200 with an empty response.
type receiver struct {
	srv        *httptest.Server
	mu         sync.Mutex
	requests   []receivedRequest
	failNext   atomic.Int32 // number of upcoming requests to answer with failWith
	failWith   atomic.Int32
	blockUntil chan struct{} // when non-nil, handlers wait for it to close before answering
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.failWith.Store(http.StatusServiceUnavailable)
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) endpoint() string { return r.srv.URL + "/v1/traces" }

func (r *receiver) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/traces" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var body io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var export coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &export); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if block := r.blockUntil; block != nil {
		<-block
	}

	r.mu.Lock()
	r.requests = append(r.requests, receivedRequest{Header: req.Header.Clone(), Body: &export})
	r.mu.Unlock()

	if r.failNext.Add(-1) >= 0 {
		http.Error(w, "nope", int(r.failWith.Load()))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	resp, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

func (r *receiver) all() []receivedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receivedRequest(nil), r.requests...)
}

func (r *receiver) stream(name string) []receivedRequest {
	var out []receivedRequest
	for _, req := range r.all() {
		if req.Header.Get(spanStreamHeader) == name {
			out = append(out, req)
		}
	}
	return out
}

func (r *receiver) spansOn(stream string) []*tracepb.Span {
	var out []*tracepb.Span
	for _, req := range r.stream(stream) {
		out = append(out, req.spans()...)
	}
	return out
}

func (r *receiver) testSpans() []*tracepb.Span { return r.spansOn(streamTest) }

func attr(attrs []*commonpb.KeyValue, key string) (*commonpb.AnyValue, bool) {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

func stringAttr(t *testing.T, attrs []*commonpb.KeyValue, key string) string {
	t.Helper()
	v, ok := attr(attrs, key)
	if !ok {
		t.Fatalf("attribute %q missing from %v", key, keys(attrs))
	}
	s, ok := v.Value.(*commonpb.AnyValue_StringValue)
	if !ok {
		t.Fatalf("attribute %q is %T, want string", key, v.Value)
	}
	return s.StringValue
}

func keys(attrs []*commonpb.KeyValue) []string {
	out := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		out = append(out, kv.Key)
	}
	return out
}

func findSpan(t *testing.T, spans []*tracepb.Span, testName string) *tracepb.Span {
	t.Helper()
	for _, s := range spans {
		if v, ok := attr(s.Attributes, attrTestName); ok && v.GetStringValue() == testName {
			return s
		}
	}
	t.Fatalf("no test.execution span for %q among %d spans", testName, len(spans))
	return nil
}

// fakeT stands in for a *testing.T so a test can drive a test execution to
// any outcome without failing itself. Cleanups run when finish is called.
type fakeT struct {
	testing.TB
	name     string
	failed   bool
	skipped  bool
	cleanups []func()
}

func newFakeT(name string) *fakeT { return &fakeT{name: name} }

func (f *fakeT) Name() string      { return f.name }
func (f *fakeT) Failed() bool      { return f.failed }
func (f *fakeT) Skipped() bool     { return f.skipped }
func (f *fakeT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) Helper()           {}
func (f *fakeT) Fail()             { f.failed = true }
func (f *fakeT) SkipNow()          { f.skipped = true }

// finish runs the registered cleanups in LIFO order, like testing does.
func (f *fakeT) finish() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
	f.cleanups = nil
}

// captureWarnings redirects collector warnings for the duration of the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	warnings.mu.Lock()
	prev := warnings.out
	prevOnce := warnings.once
	buf := &lockedBuffer{}
	warnings.out = buf
	warnings.once = map[string]bool{}
	warnings.mu.Unlock()
	t.Cleanup(func() {
		warnings.mu.Lock()
		warnings.out = prev
		warnings.once = prevOnce
		warnings.mu.Unlock()
	})
	return &buf.Buffer
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

// baseEnv clears every variable the collector reads, then applies the given
// ones, so the host CI environment cannot leak into a test.
func baseEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); collectorEnvVar(k) {
			t.Setenv(k, "") // registers the restore
			os.Unsetenv(k)  // so LookupEnv reports it absent
		}
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// collectorEnvVar reports whether the collector or the OTel SDK might read
// the variable, so tests can start from a clean environment.
func collectorEnvVar(k string) bool {
	return strings.HasPrefix(k, "BUILDKITE_") || strings.HasPrefix(k, "OTEL_") || strings.HasPrefix(k, "GITHUB_") || strings.HasPrefix(k, "CIRCLE_") || k == "CI" || k == "CI_NAME" || k == "TRACEPARENT" || k == "TRACESTATE"
}

// configured runs configure against the receiver with a valid token and run
// key, failing the test if setup does not succeed, and restores the global
// tracer provider afterwards.
func configured(t *testing.T, r *receiver, env map[string]string, opts ...Option) *collector {
	t.Helper()
	vars := map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "secret-token",
		"BUILDKITE_ANALYTICS_KEY":           "run-key-" + strings.ReplaceAll(t.Name(), "/", "_"),
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.endpoint(),
	}
	for k, v := range env {
		vars[k] = v
	}
	baseEnv(t, vars)
	restoreGlobalProvider(t)

	c, err := configure(context.Background(), newOptions(opts))
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if c == nil {
		t.Fatal("configure returned no collector")
	}
	t.Cleanup(c.shutdown)
	return c
}

func restoreGlobalProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
}

// runTest drives one fake test through Start and Cleanup with the given
// outcome applied in body.
func runTest(c *collector, name string, body func(ctx context.Context, t *fakeT)) *fakeT {
	ft := newFakeT(name)
	ctx := c.startTest(ft)
	if body != nil {
		body(ctx, ft)
	}
	ft.finish()
	return ft
}
