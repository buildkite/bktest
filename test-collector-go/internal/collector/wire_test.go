package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestAuthorizationHeaderCarriesTheTokenVerbatim(t *testing.T) {
	// The other collectors send the token as-is between the quotes; a token
	// with a quote or backslash must not be Go-escaped.
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{"BUILDKITE_TESTS_OTLP_TOKEN": `odd"to\ken`})
	run(c, "TestOne", nil)
	c.Shutdown()

	reqs := r.All()
	if len(reqs) == 0 {
		t.Fatal("no requests")
	}
	for i, req := range reqs {
		if got := req.Header.Get("Authorization"); got != `Token token="odd"to\ken"` {
			t.Errorf("request %d Authorization = %q", i, got)
		}
	}
}

func TestRequestsCarryHeadersPerStreamIncludingRetries(t *testing.T) {
	r := otlptest.NewReceiver(t)
	r.FailNext.Store(1) // the first request is answered 503 and must be retried with the same headers
	c := configured(t, r, nil)

	run(c, "TestWithChild", passes(func(ex *Execution) {
		_, child := otel.Tracer("suite").Start(ex.Context(), "db.query")
		child.End()
	}))
	c.Shutdown()

	reqs := r.All()
	if len(reqs) < 3 {
		t.Fatalf("expected the retried test request, its retry, and a child request; got %d requests", len(reqs))
	}
	for i, req := range reqs {
		for _, want := range []string{"authorization", "buildkite-tests-run-key", "buildkite-tests-span-stream"} {
			if _, ok := req.Header[http.CanonicalHeaderKey(want)]; !ok {
				t.Errorf("request %d lacks header %s: %v", i, want, req.Header)
			}
		}
		if got := req.Header.Get("Authorization"); got != `Token token="secret-token"` {
			t.Errorf("request %d Authorization = %q", i, got)
		}
		if got := req.Header.Get(runKeyHeader); got != c.env.key {
			t.Errorf("request %d run key = %q, want %q", i, got, c.env.key)
		}
		if got := req.Header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("request %d Content-Type = %q", i, got)
		}
		if got := req.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("request %d Content-Encoding = %q", i, got)
		}
		stream := req.Header.Get(spanStreamHeader)
		for _, s := range req.Spans() {
			wantStream := streamChild
			if s.Name == testSpanName {
				wantStream = streamTest
			}
			if stream != wantStream {
				t.Errorf("request %d labelled %q carries span %q", i, stream, s.Name)
			}
		}
	}
	if got := len(r.Stream(streamTest)); got != 2 {
		t.Fatalf("expected the test request and its retry, got %d test requests", got)
	}
	if len(r.TestSpans()) != 2 {
		t.Fatalf("expected the same test span in both attempts, got %d", len(r.TestSpans()))
	}
	childSpans := r.SpansOn(streamChild)
	names := map[string]bool{}
	for _, s := range childSpans {
		names[s.Name] = true
	}
	if !names["db.query"] || !names[bodyPhaseName] || len(childSpans) != 2 {
		t.Fatalf("child stream carried %v", names)
	}
}

func TestIgnoresStandardOTelEnvironment(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":            "http://127.0.0.1:1/wrong",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":     "http://127.0.0.1:1/wrong",
		"OTEL_EXPORTER_OTLP_HEADERS":             "authorization=garbage,x-env-only=1",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":      "authorization=garbage2,x-env-traces=1",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":     "http/json",
		"OTEL_EXPORTER_OTLP_PROTOCOL":            "http/json",
		"OTEL_EXPORTER_OTLP_COMPRESSION":         "none",
		"OTEL_EXPORTER_OTLP_CERTIFICATE":         "/nonexistent/ca.pem",
		"OTEL_EXPORTER_OTLP_TIMEOUT":             "1",
		"OTEL_TRACES_SAMPLER":                    "always_off",
		"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT":        "2",
		"OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT": "3",
		"OTEL_SPAN_EVENT_COUNT_LIMIT":            "0",
		"OTEL_BSP_MAX_QUEUE_SIZE":                "1",
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE":         "1",
		"OTEL_BSP_SCHEDULE_DELAY":                "600000",
	})

	for _, name := range []string{"TestA", "TestB", "TestC"} {
		run(c, name, fails(func(ex *Execution) {
			_, child := otel.Tracer("suite").Start(ex.Context(), "child")
			child.End()
			ex.Annotate("note")
		}))
	}
	c.Shutdown()

	spans := r.TestSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d test spans, want 3 (sampler/env must not apply)", len(spans))
	}
	for _, req := range r.All() {
		for k := range req.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-env") {
				t.Errorf("header %s leaked from OTEL_EXPORTER_OTLP_*HEADERS", k)
			}
		}
		if req.Header.Get("Authorization") != `Token token="secret-token"` {
			t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
		}
		if got := req.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q", got)
		}
	}
	for _, s := range spans {
		if len(s.Attributes) < 10 {
			t.Errorf("span has only %d attributes; OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT leaked", len(s.Attributes))
		}
		if got := otlptest.StringAttr(t, s.Attributes, attrTestScope); len(got) <= 3 {
			t.Errorf("scope %q truncated; OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT leaked", got)
		}
		if len(s.Events) != 1 || s.Events[0].Name != annotationEventName {
			t.Errorf("events = %v; OTEL_SPAN_EVENT_COUNT_LIMIT leaked", s.Events)
		}
	}
}

func TestEveryTestSpanIsExportedOrCountedAsDropped(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	release := make(chan struct{})
	r.BlockUntil = release
	c := configured(t, r, map[string]string{
		"BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE": "2",
		"BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE": "2",
	})
	// With the budget for waiting on queue room spent, as it is once the
	// endpoint has been unresponsive for long enough, results are queued
	// without waiting and overflow.
	c.waited.Store(int64(processorTimeout))

	const total = 6
	for i := 0; i < total; i++ {
		run(c, "TestOverflow", nil)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	c.Shutdown()

	exported := len(r.TestSpans())
	if exported >= total {
		t.Fatalf("all %d spans were exported through a queue of 2; the overflow case was not exercised", total)
	}
	want := "TEST RESULTS MISSING: OpenTelemetry dropped"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("no drop warning in:\n%s", out.String())
	}
	report, _ := c.testStats.unreported(true)
	dropped := report.dropped
	if exported+dropped != total {
		t.Fatalf("exported %d + dropped %d != %d\n%s", exported, dropped, total, out.String())
	}
	if !strings.Contains(out.String(), "(not-exported)") {
		t.Fatalf("drop reason missing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "OpenTelemetry is the only upload path") {
		t.Fatalf("second warning line missing:\n%s", out.String())
	}
	if strings.Count(out.String(), "TEST RESULTS MISSING") != 1 {
		t.Fatalf("expected one TEST RESULTS MISSING warning:\n%s", out.String())
	}
}

func TestExportFailureIsReportedWithItsStatus(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	r.FailWith.Store(http.StatusForbidden)
	r.FailNext.Store(10)
	c := configured(t, r, nil)

	run(c, "TestForbidden", nil)
	c.Shutdown()

	if got := len(r.Stream(streamTest)); got != 1 {
		t.Fatalf("403 must not be retried; got %d requests", got)
	}
	want := "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 403)."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("warning = %q\nwant it to contain %q", out.String(), want)
	}
}

// A later successful batch must not erase why an earlier one was lost.
func TestExportFailureCauseOutlivesALaterSuccess(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	r.FailWith.Store(http.StatusRequestEntityTooLarge)
	r.FailNext.Store(1)
	c := configured(t, r, map[string]string{"BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE": "1"})

	run(c, "TestTooLarge", nil)
	run(c, "TestFits", nil)
	c.Shutdown()

	if got := len(r.Stream(streamTest)); got != 2 {
		t.Fatalf("got %d requests, want one per span", got)
	}
	want := "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 413)."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("warning = %q\nwant it to contain %q", out.String(), want)
	}
}

// The exporter's error for a status it retries carries the response body
// but not the status, which the warning must still name.
func TestRetriedExportFailureIsReportedWithItsStatus(t *testing.T) {
	r := otlptest.NewReceiver(t)
	r.FailWith.Store(http.StatusServiceUnavailable)
	r.FailNext.Store(100)
	endpoint, err := url.Parse(r.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	stats := &streamStats{}
	exporter, err := newExporter(context.Background(), endpoint, "secret-token", "run-key", streamTest, stats)
	if err != nil {
		t.Fatal(err)
	}

	// The first retry waits longer than this, so the export gives up on
	// the 503 it got.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stats.offer(1)
	err = countingExporter{SpanExporter: exporter, stats: stats}.ExportSpans(ctx, tracetest.SpanStubs{{Name: "test.execution"}}.Snapshots())
	if err == nil || strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v; want the exporter's retried failure, which does not name the status", err)
	}
	if got, _ := stats.unreported(true); got.reason != "export-failure, last OTLP failure: 503" {
		t.Fatalf("reason = %q", got.reason)
	}
}

// Results that end faster than they export must wait for queue room rather
// than overflow the batch processor's queue.
func TestBurstOfResultsIsHeldBackRatherThanDropped(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	r.Delay.Store(int64(5 * time.Millisecond))
	c := configured(t, r, map[string]string{
		"BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE": "8",
		"BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE": "4",
	})
	const n = 100
	for i := range n {
		run(c, fmt.Sprintf("Test%03d", i), nil)
	}
	c.Shutdown()

	if got := len(r.TestSpans()); got != n {
		t.Errorf("got %d test spans, want %d", got, n)
	}
	if strings.Contains(out.String(), "TEST RESULTS MISSING") {
		t.Errorf("results were dropped:\n%s", out.String())
	}
}

// A server that echoes the headers it rejected must not get the token or
// the run key into the SDK's error log or the collector's warnings.
func TestExportErrorsNeverCarryTheTokenOrRunKey(t *testing.T) {
	out := captureWarnings(t)
	var sdkErrors otlptest.Buffer
	prev := otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { fmt.Fprintln(&sdkErrors, err) }))
	t.Cleanup(func() { otel.SetErrorHandler(prev) })

	r := otlptest.NewReceiver(t)
	r.FailWith.Store(http.StatusBadRequest)
	r.FailNext.Store(10)
	r.EchoHeadersInFailure.Store(true)
	c := configured(t, r, map[string]string{"BUILDKITE_TESTS_OTLP_TOKEN": "s3cret-token", "BUILDKITE_ANALYTICS_KEY": "run-key-7"})

	run(c, "TestEchoed", nil)
	c.Shutdown()

	if !strings.Contains(sdkErrors.String(), "400 Bad Request") {
		t.Fatalf("the SDK did not see the export error:\n%s", sdkErrors.String())
	}
	for _, text := range []string{sdkErrors.String(), out.String()} {
		for _, secret := range []string{"s3cret-token", "run-key-7"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q leaked:\n%s", secret, text)
			}
		}
	}
	if !strings.Contains(sdkErrors.String(), `Authorization: Token token="[REDACTED]", Buildkite-Tests-Run-Key: [REDACTED]`) {
		t.Errorf("the SDK error was not redacted in place:\n%s", sdkErrors.String())
	}

	// A one-character run key is legal and would otherwise rewrite the
	// status code and the URL of every error.
	short := redact(errors.New(`400 Bad Request: Buildkite-Tests-Run-Key: 4`), []string{"s3cret-token", "4"})
	if got := short.Error(); got != `400 Bad Request: Buildkite-Tests-Run-Key: 4` {
		t.Errorf("a short run key must be left alone, got %q", got)
	}
}

func TestUnreachableEndpointWarnsAndStaysBounded(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": otlptest.ClosedPort(t) + "/v1/traces"})

	run(c, "TestNowhere", nil)
	started := time.Now()
	c.Shutdown()
	if took := time.Since(started); took > processorTimeout {
		t.Fatalf("shutdown took %s, over the %s budget", took, processorTimeout)
	}
	if !strings.Contains(out.String(), "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: ") {
		t.Fatalf("warning = %q", out.String())
	}
}

func TestExporterPanicDoesNotKillTheWorker(t *testing.T) {
	out := captureWarnings(t)
	stats := &streamStats{}
	processor := sdktrace.NewBatchSpanProcessor(countingExporter{SpanExporter: panickingExporter{}, stats: stats})
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(countingProcessor{SpanProcessor: processor, stats: stats}))
	t.Cleanup(func() { tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("t")

	for _, round := range []int{2, 1} {
		for i := 0; i < round; i++ {
			_, span := tracer.Start(context.Background(), "s")
			span.End()
		}
		if err := tp.ForceFlush(context.Background()); err == nil || !strings.Contains(err.Error(), "exporter panicked") {
			t.Fatalf("ForceFlush err = %v, want the recovered panic", err)
		}
	}
	// The second round only reached the exporter because the worker survived
	// the first panic.
	if got, _ := stats.unreported(true); got.dropped != 3 || !strings.HasPrefix(got.reason, "export-failure, last OTLP failure: exporter panicked: boom") {
		t.Fatalf("drops = %+v, want 3 failed spans", got)
	}
	if strings.Count(out.String(), "Could not export OpenTelemetry spans: boom. Further string export failures will not be reported.") != 1 {
		t.Fatalf("warnings = %q", out.String())
	}
}

type panickingExporter struct{}

func (panickingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { panic("boom") }
func (panickingExporter) Shutdown(context.Context) error                             { return nil }

func TestExportDoesNotUseTheProcessDefaultTransport(t *testing.T) {
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the suite replaced http.DefaultTransport")
	})
	t.Cleanup(func() { http.DefaultTransport = prev })

	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	run(c, "TestOwnTransport", nil)
	c.Shutdown()
	if n := len(r.TestSpans()); n != 1 {
		t.Fatalf("got %d test spans through the collector's own transport, want 1", n)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDropAccountingSeparatesLostFromPending(t *testing.T) {
	s := &streamStats{offered: 10, succeeded: 6, failed: 1, startFailed: 1, lastFailure: "503"}

	drained, fresh := s.unreported(true)
	if !fresh || drained.dropped != 5 || drained.pending != 0 || drained.reason != "export-failure, not-exported, last OTLP failure: 503" {
		t.Fatalf("drained = %+v (%v); want 3 never exported + 1 failed + 1 start failure", drained, fresh)
	}
	if _, again := s.unreported(true); again {
		t.Fatal("the same loss was reported twice")
	}

	// Past the deadline the three unresolved spans may still be in flight,
	// so only the certain losses count as dropped.
	late, _ := s.unreported(false)
	if late.dropped != 2 || late.pending != 3 || late.reason != "export-failure, last OTLP failure: 503" {
		t.Fatalf("not drained = %+v; want 1 failed + 1 start failure dropped and 3 pending", late)
	}

	// A fresh loss does not hide the spans still in flight: every result is
	// either dropped or pending in the warnings.
	out := captureWarnings(t)
	c := &Collector{
		testStats:  &streamStats{offered: 12, failed: 10, lastFailure: "503"},
		childStats: &streamStats{offered: 5, failed: 2, lastFailure: "503"},
	}
	c.reportDrops(false, false)
	for _, want := range []string{
		"TEST RESULTS MISSING: OpenTelemetry dropped 10 test.execution span(s) (export-failure, last OTLP failure: 503).",
		"TEST RESULTS MISSING: 2 test.execution span(s) were still exporting when the 30s deadline passed",
		"OpenTelemetry dropped 2 child span(s) (export-failure, last OTLP failure: 503)",
		"3 child span(s) may be missing",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warnings lack %q:\n%s", want, out.String())
		}
	}
}

func TestProducerResourceAndRunAttributes(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{
		"BUILDKITE_BUILD_ID":               "build-uuid",
		"BUILDKITE_BUILD_URL":              "https://buildkite.com/o/p/builds/1",
		"BUILDKITE_BRANCH":                 "main",
		"BUILDKITE_COMMIT":                 "abc",
		"BUILDKITE_BUILD_NUMBER":           "1",
		"BUILDKITE_JOB_ID":                 "job-uuid",
		"BUILDKITE_MESSAGE":                "msg",
		"BUILDKITE_STEP_ID":                "step-uuid",
		"BUILDKITE_AGENT_ID":               " agent-1 ",
		"BUILDKITE_TEST_ENGINE_SUITE_SLUG": "suite",
		"BUILDKITE_ORGANIZATION_SLUG":      "org",
		"BUILDKITE_TAG":                    "v1",
		"BUILDKITE_ANALYTICS_KEY":          "build-uuid-go",
	}, withLocationPrefix("services/api/"), withTags(map[string]string{"team": "core"}))

	run(c, "TestAttrs", nil)
	c.Shutdown()

	req := r.Stream(streamTest)[0]
	res := req.Body.ResourceSpans[0].Resource.GetAttributes()
	wantRes := map[string]string{
		"service.name":               "suite",
		"service.namespace":          "org",
		"cicd.pipeline.run.id":       "build-uuid",
		"cicd.pipeline.run.url.full": "https://buildkite.com/o/p/builds/1",
		"cicd.worker.id":             " agent-1 ",
		"vcs.ref.head.name":          "main",
		"vcs.ref.head.revision":      "abc",
		"vcs.ref.type":               "tag",
	}
	for k, v := range wantRes {
		if got := otlptest.StringAttr(t, res, k); got != v {
			t.Errorf("resource %s = %q, want %q", k, got, v)
		}
	}
	if _, ok := otlptest.Attr(res, attrRunKey); ok {
		t.Error("run key must not be on the resource")
	}
	if got := otlptest.StringAttr(t, res, "process.runtime.version"); !strings.HasPrefix(got, "go") {
		t.Errorf("process.runtime.version = %q", got)
	}

	span := r.TestSpans()[0]
	wantSpan := map[string]string{
		attrRunKey:                      "build-uuid-go",
		"buildkite.build_number":        "1",
		"buildkite.job_id":              "job-uuid",
		"buildkite.message":             "msg",
		"buildkite.step_id":             "step-uuid",
		"buildkite.collector.name":      "go-buildkite-test-collector",
		"buildkite.collector.version":   Version,
		"buildkite.location_prefix":     "services/api/",
		"buildkite.test.framework.name": "gotest",
		"buildkite.tag.team":            "core",
		"buildkite.tag.ci.worker.id":    " agent-1 ",
	}
	for k, v := range wantSpan {
		if got := otlptest.StringAttr(t, span.Attributes, k); got != v {
			t.Errorf("span %s = %q, want %q", k, got, v)
		}
	}
	if got := otlptest.StringAttr(t, span.Attributes, "code.file.path"); got != "services/api/"+fixtureFile {
		t.Errorf("code.file.path = %q, want the location prefix prepended to %s", got, fixtureFile)
	}
	if _, ok := otlptest.Attr(span.Attributes, "buildkite.run_url"); ok {
		t.Error("buildkite.run_url must be omitted when it equals the CI build URL")
	}
	for _, key := range []string{"service.name", "cicd.pipeline.run.id", "vcs.ref.head.name"} {
		if _, ok := otlptest.Attr(span.Attributes, key); ok {
			t.Errorf("producer attribute %s leaked onto the test span", key)
		}
	}
}

func TestRunURLOnlyWhenItDiffersFromTheCIURL(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{
		"BUILDKITE_BUILD_ID":      "build-uuid",
		"BUILDKITE_BUILD_URL":     "https://buildkite.com/o/p/builds/1",
		"BUILDKITE_ANALYTICS_URL": "https://example.com/custom",
	})
	run(c, "TestURL", nil)
	c.Shutdown()
	span := r.TestSpans()[0]
	if got := otlptest.StringAttr(t, span.Attributes, "buildkite.run_url"); got != "https://example.com/custom" {
		t.Fatalf("buildkite.run_url = %q", got)
	}
}

func TestWorkerIDTagOmittedWhenBlankAndOverridable(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{"BUILDKITE_AGENT_ID": "  "})
	run(c, "TestBlankAgent", nil)
	c.Shutdown()
	if _, ok := otlptest.Attr(r.TestSpans()[0].Attributes, "buildkite.tag.ci.worker.id"); ok {
		t.Fatal("blank BUILDKITE_AGENT_ID must not produce a tag")
	}

	r2 := otlptest.NewReceiver(t)
	c2 := configured(t, r2, map[string]string{"BUILDKITE_AGENT_ID": "agent-9"}, withTags(map[string]string{"ci.worker.id": "pool-3"}))
	run(c2, "TestExplicitAgent", nil)
	c2.Shutdown()
	if got := otlptest.StringAttr(t, r2.TestSpans()[0].Attributes, "buildkite.tag.ci.worker.id"); got != "pool-3" {
		t.Fatalf("explicit tag must win over the automatic worker id, got %q", got)
	}
}
