package collector

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestTestSpanIsARootWithSynthesisAttributesFirst(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{
		"TRACEPARENT": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"TRACESTATE":  "bk=1",
	})

	run(c, "TestRoot/sub_case", nil)
	c.Shutdown()

	spans := r.TestSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d test spans, want 1", len(spans))
	}
	s := spans[0]
	if len(s.ParentSpanId) != 0 {
		t.Errorf("test span has a parent %x", s.ParentSpanId)
	}
	if s.Kind != tracepb.Span_SPAN_KIND_INTERNAL {
		t.Errorf("kind = %v", s.Kind)
	}
	if s.Name != testSpanName {
		t.Errorf("name = %q", s.Name)
	}
	if len(s.Links) != 1 {
		t.Fatalf("links = %v, want one link to the job span", s.Links)
	}
	if got := s.Links[0].TraceId; strings.ToLower(hexOf(got)) != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("link trace id = %x", got)
	}
	if got := s.Links[0].SpanId; hexOf(got) != "b7ad6b7169203331" {
		t.Errorf("link span id = %x", got)
	}
	if got := otlptest.Keys(s.Attributes)[:3]; strings.Join(got, ",") != "buildkite.execution.via,buildkite.run_key,test.case.result.status" {
		t.Errorf("first attributes = %v", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, attrExecutionVia); got != "otlp" {
		t.Errorf("via = %q", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, attrRunKey); got != c.env.key {
		t.Errorf("run key attr = %q, want %q", got, c.env.key)
	}
	if got := otlptest.StringAttr(t, s.Attributes, "test.case.result.status"); got != "pass" {
		t.Errorf("status = %q", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, attrTestName); got != "TestRoot/sub_case" {
		t.Errorf("name attr = %q", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, attrTestScope); got != fixtureScope || otlptest.StringAttr(t, s.Attributes, "test.suite.name") != fixtureScope {
		t.Errorf("scope = %q", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, "test.case.name"); got != fixtureScope+".TestRoot/sub_case" {
		t.Errorf("test.case.name = %q", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, "code.file.path"); got != fixtureFile {
		t.Errorf("code.file.path = %q", got)
	}
	if got := otlptest.IntAttr(t, s.Attributes, "code.line.number"); got != 10 {
		t.Errorf("code.line.number = %d", got)
	}
	uuid7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if got := otlptest.StringAttr(t, s.Attributes, attrExternalID); !uuid7.MatchString(got) {
		t.Errorf("external id = %q, want a UUIDv7", got)
	}
	if s.StartTimeUnixNano == 0 || s.EndTimeUnixNano < s.StartTimeUnixNano {
		t.Errorf("start %d end %d", s.StartTimeUnixNano, s.EndTimeUnixNano)
	}
	if s.Status.GetCode() != tracepb.Status_STATUS_CODE_UNSET {
		t.Errorf("passing span status = %v", s.Status)
	}
	if len(s.Events) != 0 {
		t.Errorf("passing span has events %v", s.Events)
	}
}

func TestUnknownLocationOmitsCodeAttributes(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	c.Start(Identity{Scope: fixtureScope, Name: "TestNoFile"}, time.Now()).Finish(time.Now(), passed)
	c.Start(Identity{Scope: fixtureScope, Name: "TestNoLine", File: fixtureFile}, time.Now()).Finish(time.Now(), passed)
	c.Shutdown()

	noFile := otlptest.FindSpan(t, r.TestSpans(), "TestNoFile")
	for _, key := range []string{"code.file.path", "code.line.number"} {
		if _, ok := otlptest.Attr(noFile.Attributes, key); ok {
			t.Errorf("%s set for an execution without a location", key)
		}
	}
	noLine := otlptest.FindSpan(t, r.TestSpans(), "TestNoLine")
	if otlptest.StringAttr(t, noLine.Attributes, "code.file.path") != fixtureFile {
		t.Error("file missing")
	}
	if _, ok := otlptest.Attr(noLine.Attributes, "code.line.number"); ok {
		t.Error("code.line.number set without a line")
	}
}

func TestMalformedTraceContextProducesNoLink(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{"TRACEPARENT": "garbage", "TRACESTATE": "x"})
	run(c, "TestNoLink", nil)
	c.Shutdown()
	if links := r.TestSpans()[0].Links; len(links) != 0 {
		t.Fatalf("links = %v, want none", links)
	}
}

func TestResultMapping(t *testing.T) {
	long := strings.Repeat("x", FailureReasonLength+100)
	cases := []struct {
		name   string
		result Result
		status string
		code   tracepb.Status_StatusCode
		reason string
	}{
		{"passed", passed, "pass", tracepb.Status_STATUS_CODE_UNSET, ""},
		{"failed", failed, "fail", tracepb.Status_STATUS_CODE_ERROR, "test failed"},
		{"skipped", skipped, "skipped", tracepb.Status_STATUS_CODE_UNSET, ""},
		{"long reason", Result{Status: StatusFail, Reason: long}, "fail", tracepb.Status_STATUS_CODE_ERROR, long[:FailureReasonLength-3] + "..."},
	}
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	for _, tc := range cases {
		run(c, tc.name, func(*Execution) Result { return tc.result })
	}
	c.Shutdown()

	spans := r.TestSpans()
	if len(spans) != len(cases) {
		t.Fatalf("got %d spans, want %d", len(spans), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := otlptest.FindSpan(t, spans, tc.name)
			if got := otlptest.StringAttr(t, s.Attributes, "test.case.result.status"); got != tc.status {
				t.Errorf("status attr = %q, want %q", got, tc.status)
			}
			if s.Status.GetCode() != tc.code || s.Status.GetMessage() != tc.reason {
				t.Errorf("span status = %v, want %v %q", s.Status, tc.code, tc.reason)
			}
			if len(s.Events) != 0 {
				t.Errorf("a failure without an exception has no event; got %v", s.Events)
			}
		})
	}
}

func TestExceptionEventCarriesTruncatedDetail(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	message := strings.Repeat("m", exceptionMessageLength+1)
	stack := strings.Repeat("s", testSpanEventAttributeLength+1)
	run(c, "TestPanicked", func(*Execution) Result {
		return Result{Status: StatusFail, Reason: "test panicked", Exception: &Exception{Type: "panic", Message: message, Stacktrace: stack}}
	})
	run(c, "TestTypeOnly", func(*Execution) Result {
		return Result{Status: StatusFail, Reason: "boom", Exception: &Exception{Type: "panic"}}
	})
	c.Shutdown()

	s := otlptest.FindSpan(t, r.TestSpans(), "TestPanicked")
	if len(s.Events) != 1 || s.Events[0].Name != "exception" {
		t.Fatalf("events = %v", s.Events)
	}
	ev := s.Events[0]
	if got := otlptest.StringAttr(t, ev.Attributes, "exception.type"); got != "panic" {
		t.Errorf("exception.type = %q", got)
	}
	if got := otlptest.StringAttr(t, ev.Attributes, "exception.message"); len(got) != exceptionMessageLength || !strings.HasSuffix(got, "...") {
		t.Errorf("exception.message has %d characters, want %d with a truncation marker", len(got), exceptionMessageLength)
	}
	if got := otlptest.StringAttr(t, ev.Attributes, "exception.stacktrace"); len(got) != testSpanEventAttributeLength || !strings.HasSuffix(got, "...") {
		t.Errorf("exception.stacktrace has %d characters, want %d with a truncation marker", len(got), testSpanEventAttributeLength)
	}
	// Failed tests export their body phase with the same failure recorded.
	bodies := r.SpansOn(streamChild)
	if len(bodies) != 2 {
		t.Fatalf("child stream = %v", r.ChildNames())
	}
	for _, b := range bodies {
		if b.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || len(b.Events) != 1 {
			t.Errorf("body phase status = %v events = %d", b.Status, len(b.Events))
		}
	}

	typeOnly := otlptest.FindSpan(t, r.TestSpans(), "TestTypeOnly")
	if len(typeOnly.Events) != 1 || len(typeOnly.Events[0].Attributes) != 1 {
		t.Fatalf("empty message and stacktrace must be omitted, got %v", typeOnly.Events)
	}
}

func TestEndTimeFallsBackToSDKClockWhenItPrecedesStart(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)

	start := time.Now()
	ex := c.Start(Identity{Scope: fixtureScope, Name: "TestClock"}, start)
	ex.Finish(start.Add(-time.Hour), passed) // the clock stepped backwards
	c.Shutdown()

	s := r.TestSpans()[0]
	if s.StartTimeUnixNano != uint64(start.UnixNano()) {
		t.Fatalf("start = %d, want %d", s.StartTimeUnixNano, start.UnixNano())
	}
	if s.EndTimeUnixNano < s.StartTimeUnixNano {
		t.Fatalf("end %d precedes start %d; the SDK clock fallback did not apply", s.EndTimeUnixNano, s.StartTimeUnixNano)
	}
}

func TestExplicitTimesAreKept(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)

	start := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	c.Start(Identity{Scope: fixtureScope, Name: "TestTimes"}, start).Finish(end, passed)
	c.Shutdown()

	s := r.TestSpans()[0]
	if s.StartTimeUnixNano != uint64(start.UnixNano()) || s.EndTimeUnixNano != uint64(end.UnixNano()) {
		t.Fatalf("span times %d..%d, want %d..%d", s.StartTimeUnixNano, s.EndTimeUnixNano, start.UnixNano(), end.UnixNano())
	}
}

func TestIdentityWithInvalidUTF8IsRepaired(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	c.Start(Identity{Scope: "example.com/bad\xff", Name: "Test\xffName", File: "x\xff_test.go", Line: 1}, time.Now()).Finish(time.Now(), passed)
	c.Shutdown()

	spans := r.TestSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans; the batch must still encode", len(spans))
	}
	if got := otlptest.StringAttr(t, spans[0].Attributes, attrTestName); got != "Test\uFFFDName" {
		t.Errorf("name = %q", got)
	}
	if got := otlptest.StringAttr(t, spans[0].Attributes, attrTestScope); got != "example.com/bad\uFFFD" {
		t.Errorf("scope = %q", got)
	}
	if got := otlptest.StringAttr(t, spans[0].Attributes, "code.file.path"); got != "x\uFFFD_test.go" {
		t.Errorf("file = %q", got)
	}
}

func TestTagsMergeWithPerTestWinning(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, map[string]string{"BUILDKITE_AGENT_ID": "agent-1"}, withTags(map[string]string{"team": "core", "shard": "1"}))
	run(c, "TestTags", passes(func(ex *Execution) {
		ex.Tag("shard", "7")
		ex.Tag("owner", "me\xff")
	}))
	c.Shutdown()

	s := r.TestSpans()[0]
	want := map[string]string{
		"buildkite.tag.team":         "core",
		"buildkite.tag.shard":        "7",
		"buildkite.tag.owner":        "me\uFFFD",
		"buildkite.tag.ci.worker.id": "agent-1",
	}
	for k, v := range want {
		if got := otlptest.StringAttr(t, s.Attributes, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Tags come after the identity and run attributes, so an attribute-count
	// overflow would drop tags before anything the server needs.
	ks := otlptest.Keys(s.Attributes)
	firstTag, lastNonTag := -1, -1
	for i, k := range ks {
		if strings.HasPrefix(k, tagAttributePrefix) {
			if firstTag < 0 {
				firstTag = i
			}
		} else {
			lastNonTag = i
		}
	}
	if firstTag < lastNonTag {
		t.Errorf("tags are interleaved with other attributes: %v", ks)
	}
}

func TestAnnotateTargetsTheTestSpanNotTheCurrentSpan(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	run(c, "TestAnnotate", passes(func(ex *Execution) {
		_, inner := otel.Tracer("suite").Start(ex.Context(), "inner")
		ex.Annotate("first")
		ex.Annotate("second\xff")
		inner.End()
	}))
	c.Shutdown()

	s := r.TestSpans()[0]
	if len(s.Events) != 2 || s.Events[0].Name != annotationEventName || otlptest.StringAttr(t, s.Events[0].Attributes, attrAnnotation) != "first" || otlptest.StringAttr(t, s.Events[1].Attributes, attrAnnotation) != "second\uFFFD" {
		t.Fatalf("events = %v", s.Events)
	}
	for _, child := range r.SpansOn(streamChild) {
		if len(child.Events) != 0 {
			t.Errorf("annotation landed on child span %q", child.Name)
		}
	}
}

func TestAnnotationIsCappedLikeOtherEventStrings(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	long := strings.Repeat("é", testSpanEventAttributeLength+1)
	run(c, "TestLongAnnotation", passes(func(ex *Execution) { ex.Annotate(long) }))
	c.Shutdown()

	got := otlptest.StringAttr(t, r.TestSpans()[0].Events[0].Attributes, attrAnnotation)
	if want := strings.Repeat("é", testSpanEventAttributeLength-3) + "..."; got != want {
		t.Fatalf("annotation has %d characters, want %d with a truncation marker", len([]rune(got)), testSpanEventAttributeLength)
	}
}

func TestWithoutChildSpansNothingUsesTheGlobalProvider(t *testing.T) {
	r := otlptest.NewReceiver(t)
	otlptest.ClearEnv(t, map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "secret-token",
		"BUILDKITE_ANALYTICS_KEY":           "run-key",
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.Endpoint(),
	})
	restoreGlobalProvider(t)
	before := otel.GetTracerProvider()
	c, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before {
		t.Fatalf("global provider replaced with %T", otel.GetTracerProvider())
	}
	ex := c.Start(Identity{Scope: fixtureScope, Name: "TestNoChildren"}, time.Now())
	if trace.SpanContextFromContext(ex.Context()).SpanID() != trace.SpanFromContext(ex.Context()).SpanContext().SpanID() {
		t.Fatal("context carries a span that is not the current one")
	}
	_, child := otel.Tracer("suite").Start(ex.Context(), "ignored")
	child.End()
	ex.Finish(time.Now(), failed)
	c.Shutdown()

	if len(r.TestSpans()) != 1 {
		t.Fatalf("test spans = %v", otlptest.TestNames(r.TestSpans()))
	}
	if n := len(r.Stream(streamChild)); n != 0 {
		t.Fatalf("%d child requests sent with child spans disabled", n)
	}
}

// A suite provider that panics costs the body phase, never the result.
func TestPhaseStartFailureKeepsTheTestResult(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil, withTracerProvider(explodingProvider{}))
	if !strings.Contains(out.String(), "OpenTelemetry child span export disabled: tracer provider collector.explodingProvider panicked while registering the span processor: no processors here; test.execution export remains enabled") {
		t.Fatalf("setup warning = %q", out.String())
	}

	run(c, "TestNoPhase", func(ex *Execution) Result {
		if ExecutionFromContext(ex.Context()) != ex {
			t.Error("Start must still hand back the execution context")
		}
		return failed
	})
	c.Shutdown()

	if !strings.Contains(out.String(), "Could not start the OpenTelemetry test.body phase span: no tracer either") {
		t.Fatalf("phase warning = %q", out.String())
	}
	spans := r.TestSpans()
	if len(spans) != 1 || otlptest.StringAttr(t, spans[0].Attributes, "test.case.result.status") != string(StatusFail) {
		t.Fatalf("test spans = %v", otlptest.TestNames(spans))
	}
	if strings.Contains(out.String(), "TEST RESULTS MISSING") {
		t.Fatalf("the result was delivered, nothing is missing:\n%s", out.String())
	}
}

type explodingProvider struct{ trace.TracerProvider }

func (explodingProvider) RegisterSpanProcessor(sdktrace.SpanProcessor) { panic("no processors here") }
func (explodingProvider) Tracer(string, ...trace.TracerOption) trace.Tracer {
	panic("no tracer either")
}

func TestStartFailureIsCountedAsAMissingResult(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	c.testTracer = panickingTracer{}

	if ex := c.Start(Identity{Scope: fixtureScope, Name: "TestBroken"}, time.Now()); ex != nil {
		t.Fatal("a failed start must return nil")
	}
	c.Shutdown()

	if !strings.Contains(out.String(), "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (could not start span: tracer exploded)") {
		t.Fatalf("warning = %q", out.String())
	}
	if !strings.Contains(out.String(), "OpenTelemetry is the only upload path") {
		t.Fatalf("second line missing: %q", out.String())
	}
	if n := strings.Count(out.String(), "TEST RESULTS MISSING"); n != 1 {
		t.Fatalf("a start failure was reported %d times, want once (not again at shutdown):\n%s", n, out.String())
	}
}

type panickingTracer struct{ trace.Tracer }

func (panickingTracer) Start(context.Context, string, ...trace.SpanStartOption) (context.Context, trace.Span) {
	panic("tracer exploded")
}

func TestExportedCountsAcceptedTestSpans(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	run(c, "TestOne", nil)
	run(c, "TestTwo", fails(func(ex *Execution) {
		_, child := otel.Tracer("suite").Start(ex.Context(), "child")
		child.End()
	}))
	c.Shutdown()
	if got := c.Exported(); got != 2 {
		t.Fatalf("Exported() = %d, want 2 (child spans do not count)", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("é", 20), 10); got != strings.Repeat("é", 7)+"..." {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate(strings.Repeat("x", 10), 10); got != strings.Repeat("x", 10) {
		t.Errorf("truncate at the limit = %q", got)
	}
}
