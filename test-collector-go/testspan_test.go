package bktest

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestTestSpanIsARootWithSynthesisAttributesFirst(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, map[string]string{
		"TRACEPARENT": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"TRACESTATE":  "bk=1",
	})

	runTest(c, "TestRoot/sub_case", nil)
	c.shutdown()

	spans := r.testSpans()
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
	if got := keys(s.Attributes)[:3]; strings.Join(got, ",") != "buildkite.execution.via,buildkite.run_key,test.case.result.status" {
		t.Errorf("first attributes = %v", got)
	}
	if got := stringAttr(t, s.Attributes, attrExecutionVia); got != "otlp" {
		t.Errorf("via = %q", got)
	}
	if got := stringAttr(t, s.Attributes, attrRunKey); got != c.env.key {
		t.Errorf("run key attr = %q, want %q", got, c.env.key)
	}
	if got := stringAttr(t, s.Attributes, "test.case.result.status"); got != "pass" {
		t.Errorf("status = %q", got)
	}
	if got := stringAttr(t, s.Attributes, attrTestName); got != "TestRoot/sub_case" {
		t.Errorf("name attr = %q", got)
	}
	if scope := stringAttr(t, s.Attributes, attrTestScope); scope == "" || stringAttr(t, s.Attributes, "test.suite.name") != scope {
		t.Errorf("scope = %q", scope)
	}
	if v, ok := attr(s.Attributes, "code.line.number"); !ok || v.GetIntValue() <= 0 {
		t.Errorf("code.line.number = %v, want a positive int", v)
	}
	uuid7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if got := stringAttr(t, s.Attributes, attrExternalID); !uuid7.MatchString(got) {
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

func TestMalformedTraceContextProducesNoLink(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, map[string]string{"TRACEPARENT": "garbage", "TRACESTATE": "x"})
	runTest(c, "TestNoLink", nil)
	c.shutdown()
	if links := r.testSpans()[0].Links; len(links) != 0 {
		t.Fatalf("links = %v, want none", links)
	}
}

func TestResultMapping(t *testing.T) {
	cases := []struct {
		name   string
		drive  func(ft *fakeT)
		status string
		code   tracepb.Status_StatusCode
		reason string
	}{
		{"passed", nil, "pass", tracepb.Status_STATUS_CODE_UNSET, ""},
		{"failed", func(ft *fakeT) { ft.Fail() }, "fail", tracepb.Status_STATUS_CODE_ERROR, "test failed"},
		{"skipped", func(ft *fakeT) { ft.SkipNow() }, "skipped", tracepb.Status_STATUS_CODE_UNSET, ""},
		{"failed then skipped", func(ft *fakeT) { ft.Fail(); ft.SkipNow() }, "fail", tracepb.Status_STATUS_CODE_ERROR, "test failed"},
	}
	r := newReceiver(t)
	c := configured(t, r, nil)
	for _, tc := range cases {
		runTest(c, tc.name, func(_ context.Context, ft *fakeT) {
			if tc.drive != nil {
				tc.drive(ft)
			}
		})
	}
	c.shutdown()

	spans := r.testSpans()
	if len(spans) != len(cases) {
		t.Fatalf("got %d spans, want %d", len(spans), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := findSpan(t, spans, tc.name)
			if got := stringAttr(t, s.Attributes, "test.case.result.status"); got != tc.status {
				t.Errorf("status attr = %q, want %q", got, tc.status)
			}
			if s.Status.GetCode() != tc.code || s.Status.GetMessage() != tc.reason {
				t.Errorf("span status = %v, want %v %q", s.Status, tc.code, tc.reason)
			}
			if len(s.Events) != 0 {
				t.Errorf("a non-panic failure has no failure text, so no exception event; got %v", s.Events)
			}
		})
	}
}

func TestEndTimeFallsBackToSDKClockWhenItPrecedesStart(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, nil)
	realNow := now
	t.Cleanup(func() { now = realNow })

	base := time.Now()
	calls := 0
	now = func() time.Time {
		calls++
		if calls == 1 {
			return base // span start
		}
		return base.Add(-time.Hour) // the clock stepped backwards before Cleanup
	}
	runTest(c, "TestClock", nil)
	now = realNow
	c.shutdown()

	s := r.testSpans()[0]
	if s.StartTimeUnixNano != uint64(base.UnixNano()) {
		t.Fatalf("start = %d, want %d", s.StartTimeUnixNano, base.UnixNano())
	}
	if s.EndTimeUnixNano < s.StartTimeUnixNano {
		t.Fatalf("end %d precedes start %d; the SDK clock fallback did not apply", s.EndTimeUnixNano, s.StartTimeUnixNano)
	}
}

func TestTagsMergeWithPerTestWinning(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, map[string]string{"BUILDKITE_AGENT_ID": "agent-1"}, WithTags(map[string]string{"team": "core", "shard": "1"}))
	runTest(c, "TestTags", func(ctx context.Context, ft *fakeT) {
		Tag(ctx, "shard", "7")
		Tag(ctx, "owner", "me\xff")
	})
	c.shutdown()

	s := r.testSpans()[0]
	want := map[string]string{
		"buildkite.tag.team":         "core",
		"buildkite.tag.shard":        "7",
		"buildkite.tag.owner":        "me\uFFFD",
		"buildkite.tag.ci.worker.id": "agent-1",
	}
	for k, v := range want {
		if got := stringAttr(t, s.Attributes, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Tags come after the identity and run attributes, so an attribute-count
	// overflow would drop tags before anything the server needs.
	ks := keys(s.Attributes)
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

func TestTagAndAnnotateOutsideATestWarnOnce(t *testing.T) {
	out := captureWarnings(t)
	Tag(context.Background(), "silent", "export is disabled, so nothing to warn about")
	Annotate(context.Background(), "silent")
	if out.Len() != 0 {
		t.Fatalf("warned while disabled:\n%s", out.String())
	}

	current.Store(configured(t, newReceiver(t), nil))
	t.Cleanup(func() { current.Store(nil) })
	Tag(context.Background(), "k", "v")
	Tag(context.Background(), "k2", "v")
	Annotate(context.Background(), "x")
	Annotate(context.Background(), "y")
	var nilCtx context.Context
	Tag(nilCtx, "k", "v") // a nil context must not panic
	if n := strings.Count(out.String(), "bktest.Tag was called"); n != 1 {
		t.Errorf("Tag warned %d times:\n%s", n, out.String())
	}
	if n := strings.Count(out.String(), "bktest.Annotate was called"); n != 1 {
		t.Errorf("Annotate warned %d times:\n%s", n, out.String())
	}
}

func TestAnnotateTargetsTheTestSpanNotTheCurrentSpan(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, nil)
	runTest(c, "TestAnnotate", func(ctx context.Context, ft *fakeT) {
		ctx, inner := otel.Tracer("suite").Start(ctx, "inner")
		Annotate(ctx, "first")
		Annotate(ctx, "second")
		inner.End()
	})
	c.shutdown()

	s := r.testSpans()[0]
	if len(s.Events) != 2 || s.Events[0].Name != annotationEventName || stringAttr(t, s.Events[0].Attributes, attrAnnotation) != "first" || stringAttr(t, s.Events[1].Attributes, attrAnnotation) != "second" {
		t.Fatalf("events = %v", s.Events)
	}
	for _, child := range r.spansOn(streamChild) {
		if len(child.Events) != 0 {
			t.Errorf("annotation landed on child span %q", child.Name)
		}
	}
}

func TestAnnotationIsCappedLikeOtherEventStrings(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, nil)
	long := strings.Repeat("é", testSpanEventAttributeLength+1)
	runTest(c, "TestLongAnnotation", func(ctx context.Context, ft *fakeT) { Annotate(ctx, long) })
	c.shutdown()

	got := stringAttr(t, r.testSpans()[0].Events[0].Attributes, attrAnnotation)
	if want := strings.Repeat("é", testSpanEventAttributeLength-3) + "..."; got != want {
		t.Fatalf("annotation has %d characters, want %d with a truncation marker", len([]rune(got)), testSpanEventAttributeLength)
	}
}

// A suite provider that panics costs the body phase, never the result.
func TestPhaseStartFailureKeepsTheTestResult(t *testing.T) {
	out := captureWarnings(t)
	r := newReceiver(t)
	c := configured(t, r, nil, WithTracerProvider(explodingProvider{}))
	if !strings.Contains(out.String(), "OpenTelemetry child span export disabled: tracer provider bktest.explodingProvider panicked while registering the span processor: no processors here; test.execution export remains enabled") {
		t.Fatalf("setup warning = %q", out.String())
	}

	ft := runTest(c, "TestNoPhase", func(ctx context.Context, ft *fakeT) {
		if executionFromContext(ctx) == nil {
			t.Error("Start must still hand back the execution context")
		}
		ft.Fail()
	})
	c.shutdown()

	if !strings.Contains(out.String(), "Could not start the OpenTelemetry test.body phase span: no tracer either") {
		t.Fatalf("phase warning = %q", out.String())
	}
	if ft.cleanups != nil {
		t.Fatal("cleanup did not run")
	}
	spans := r.testSpans()
	if len(spans) != 1 || stringAttr(t, spans[0].Attributes, "test.case.result.status") != resultFail {
		t.Fatalf("test spans = %v", testNames(spans))
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

func TestStartTwiceReturnsTheSameExecution(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, nil)
	ft := newFakeT("TestTwice")
	ctx1 := c.startTest(ft)
	ctx2 := c.startTest(ft)
	if ctx1 != ctx2 {
		t.Fatal("second Start returned a different context")
	}
	ft.finish()
	c.shutdown()
	if n := len(r.testSpans()); n != 1 {
		t.Fatalf("got %d test spans, want 1", n)
	}
}

func TestStartFailureIsCountedAsAMissingResult(t *testing.T) {
	out := captureWarnings(t)
	r := newReceiver(t)
	c := configured(t, r, nil)
	c.testTracer = panickingTracer{}

	ft := newFakeT("TestBroken")
	ctx := c.startTest(ft)
	if ctx == nil || executionFromContext(ctx) != nil {
		t.Fatal("a failed start must hand back a plain context")
	}
	ft.finish()
	c.shutdown()

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

func TestPackagePath(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/widgets/calculator.TestAdd":              "github.com/acme/widgets/calculator",
		"github.com/acme/widgets/calculator_test.TestAdd.func1":   "github.com/acme/widgets/calculator",
		"github.com/acme/widgets/v2.TestX":                        "github.com/acme/widgets/v2",
		"github.com/acme/go-collector/pkg.TestGeneric[...].func2": "github.com/acme/go-collector/pkg",
		"main.TestMain": "main",
		"github.com/acme/widgets/calculator.(*Suite).TestMethod":      "github.com/acme/widgets/calculator",
		"github.com/acme/dotted.name/pkg_test.TestDottedModule.func1": "github.com/acme/dotted.name/pkg",
		"gopkg.in/yaml%2ev3.TestUnmarshal":                            "gopkg.in/yaml.v3",
		"gopkg.in/yaml%2ev3_test.TestUnmarshal.func2":                 "gopkg.in/yaml.v3",
	}
	for in, want := range cases {
		if got := packagePath(in); got != want {
			t.Errorf("packagePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModuleRelativePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleRelative(filepath.Join(root, "pkg", "sub", "x_test.go")); got != "pkg/sub/x_test.go" {
		t.Errorf("moduleRelative = %q", got)
	}
	// -trimpath records the module path in place of the module root; this
	// test binary's module is the collector itself, at its root.
	if got := moduleRelative(collectorPackagePath + "/x_test.go"); got != "x_test.go" {
		t.Errorf("trimpath form = %q, want x_test.go", got)
	}
	if got := moduleRelative("example.com/other/pkg/x_test.go"); got != "example.com/other/pkg/x_test.go" {
		t.Errorf("a file from another module changed to %q", got)
	}
	if got := moduleRelative(""); got != "" {
		t.Errorf("empty = %q", got)
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

func hexOf(b []byte) string { return hex.EncodeToString(b) }
