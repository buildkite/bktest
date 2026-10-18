// Package otlptest is the test harness shared by the collector's packages:
// a loopback OTLP receiver, attribute lookups on the decoded spans, and
// helpers that isolate a test from the host's CI environment.
package otlptest

import (
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	StreamHeader = "Buildkite-Tests-Span-Stream"
	RunKeyHeader = "Buildkite-Tests-Run-Key"
	StreamTest   = "test"
	StreamChild  = "child"
	TestNameAttr = "buildkite.test.name"
)

// Request is one OTLP export request as the receiver saw it.
type Request struct {
	Header http.Header
	Body   *coltracepb.ExportTraceServiceRequest
}

// Spans flattens the request's spans.
func (r Request) Spans() []*tracepb.Span {
	var out []*tracepb.Span
	for _, rs := range r.Body.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			out = append(out, ss.Spans...)
		}
	}
	return out
}

// Receiver is a loopback stand-in for tests-otlp.buildkite.com: it gunzips
// and decodes every request, records it, and answers with the configured
// status, or 200 with an empty response.
type Receiver struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []Request
	// FailNext is the number of upcoming requests to answer with FailWith.
	FailNext atomic.Int32
	FailWith atomic.Int32
	// BlockUntil, when non-nil, makes handlers wait for it to close before
	// answering.
	BlockUntil chan struct{}
}

// NewReceiver starts a receiver that is closed when the test ends.
func NewReceiver(t testing.TB) *Receiver {
	t.Helper()
	r := &Receiver{}
	r.FailWith.Store(http.StatusServiceUnavailable)
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

// Endpoint is the receiver's /v1/traces URL.
func (r *Receiver) Endpoint() string { return r.srv.URL + "/v1/traces" }

func (r *Receiver) handle(w http.ResponseWriter, req *http.Request) {
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

	if block := r.BlockUntil; block != nil {
		<-block
	}

	r.mu.Lock()
	r.requests = append(r.requests, Request{Header: req.Header.Clone(), Body: &export})
	r.mu.Unlock()

	if r.FailNext.Add(-1) >= 0 {
		http.Error(w, "nope", int(r.FailWith.Load()))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	resp, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// All returns every request received so far.
func (r *Receiver) All() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.requests...)
}

// Stream returns the requests labelled with the given span stream.
func (r *Receiver) Stream(name string) []Request {
	var out []Request
	for _, req := range r.All() {
		if req.Header.Get(StreamHeader) == name {
			out = append(out, req)
		}
	}
	return out
}

// SpansOn returns every span received on the given stream.
func (r *Receiver) SpansOn(stream string) []*tracepb.Span {
	var out []*tracepb.Span
	for _, req := range r.Stream(stream) {
		out = append(out, req.Spans()...)
	}
	return out
}

// TestSpans returns every span received on the test stream.
func (r *Receiver) TestSpans() []*tracepb.Span { return r.SpansOn(StreamTest) }

// ChildNames counts the spans on the child stream by name.
func (r *Receiver) ChildNames() map[string]int {
	names := map[string]int{}
	for _, s := range r.SpansOn(StreamChild) {
		names[s.Name]++
	}
	return names
}

// Attr looks up an attribute by key.
func Attr(attrs []*commonpb.KeyValue, key string) (*commonpb.AnyValue, bool) {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// StringAttr returns a string attribute, failing the test if it is missing
// or not a string.
func StringAttr(t testing.TB, attrs []*commonpb.KeyValue, key string) string {
	t.Helper()
	v, ok := Attr(attrs, key)
	if !ok {
		t.Fatalf("attribute %q missing from %v", key, Keys(attrs))
	}
	s, ok := v.Value.(*commonpb.AnyValue_StringValue)
	if !ok {
		t.Fatalf("attribute %q is %T, want string", key, v.Value)
	}
	return s.StringValue
}

// IntAttr returns an int attribute, failing the test if it is missing.
func IntAttr(t testing.TB, attrs []*commonpb.KeyValue, key string) int {
	t.Helper()
	v, ok := Attr(attrs, key)
	if !ok {
		t.Fatalf("attribute %q missing from %v", key, Keys(attrs))
	}
	return int(v.GetIntValue())
}

// Keys lists attribute keys in order.
func Keys(attrs []*commonpb.KeyValue) []string {
	out := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		out = append(out, kv.Key)
	}
	return out
}

// FindSpan returns the test.execution span for the named test.
func FindSpan(t testing.TB, spans []*tracepb.Span, testName string) *tracepb.Span {
	t.Helper()
	for _, s := range spans {
		if v, ok := Attr(s.Attributes, TestNameAttr); ok && v.GetStringValue() == testName {
			return s
		}
	}
	t.Fatalf("no test.execution span for %q among %d spans: %v", testName, len(spans), TestNames(spans))
	return nil
}

// TestNames lists the test names of the given spans.
func TestNames(spans []*tracepb.Span) []string {
	var names []string
	for _, s := range spans {
		if v, ok := Attr(s.Attributes, TestNameAttr); ok {
			names = append(names, v.GetStringValue())
		} else {
			names = append(names, "<"+s.Name+">")
		}
	}
	return names
}

// Buffer is a bytes.Buffer safe for concurrent writes, for capturing the
// collector's warnings while its exporter goroutines are still running.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// ClearEnv clears every variable the collector reads, then applies the
// given ones, so the host CI environment cannot leak into a test.
func ClearEnv(t testing.TB, vars map[string]string) {
	t.Helper()
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); CollectorEnvVar(k) {
			t.Setenv(k, "") // registers the restore
			os.Unsetenv(k)  // so LookupEnv reports it absent
		}
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// CollectorEnvVar reports whether the collector or the OTel SDK might read
// the variable, so tests can start from a clean environment.
func CollectorEnvVar(k string) bool {
	return strings.HasPrefix(k, "BUILDKITE_") || strings.HasPrefix(k, "OTEL_") || strings.HasPrefix(k, "GITHUB_") || strings.HasPrefix(k, "CIRCLE_") || k == "CI" || k == "CI_NAME" || k == "TRACEPARENT" || k == "TRACESTATE"
}

// ClosedPort returns a loopback URL nothing listens on.
func ClosedPort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}
