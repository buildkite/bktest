package collector

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestForwardsOnlySpansFromTheExecutionTrace(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	tracer := otel.Tracer("suite")

	run(c, "TestChildren", passes(func(ex *Execution) {
		ctx, parent := tracer.Start(ex.Context(), "parent")
		_, grandchild := tracer.Start(ctx, "grandchild")
		grandchild.End()
		parent.End()

		_, outside := tracer.Start(context.Background(), "outside-the-test")
		outside.End()

		// Same context key, different trace: a customer root span inside a test.
		_, otherRoot := tracer.Start(ctx, "other-root", trace.WithNewRoot())
		otherRoot.End()
	}))
	c.Shutdown()

	got := r.ChildNames()
	want := map[string]int{"parent": 1, "grandchild": 1, bodyPhaseName: 1}
	if len(got) != len(want) {
		t.Fatalf("child stream = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("child stream %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	for _, s := range r.SpansOn(streamChild) {
		if s.Name == bodyPhaseName && hexOf(s.ParentSpanId) != hexOf(r.TestSpans()[0].SpanId) {
			t.Errorf("body phase parent = %x, want the test span %x", s.ParentSpanId, r.TestSpans()[0].SpanId)
		}
		if s.Name == "parent" && hexOf(s.ParentSpanId) == hexOf(r.TestSpans()[0].SpanId) {
			t.Errorf("instrumentation should nest under the body phase, not directly under the test span")
		}
	}
}

func TestPhaseExportRule(t *testing.T) {
	r := otlptest.NewReceiver(t)
	dropAll := func(sdktrace.ReadOnlySpan) bool { return false }
	c := configured(t, r, nil, withFilter(dropAll))
	tracer := otel.Tracer("suite")

	run(c, "passing with nothing", nil)
	run(c, "failing with nothing", fails(func(*Execution) {}))
	run(c, "passing with a child", passes(func(ex *Execution) {
		_, child := tracer.Start(ex.Context(), "child")
		child.End()
	}))
	run(c, "failing with a child", func(ex *Execution) Result {
		_, child := tracer.Start(ex.Context(), "child")
		child.End()
		return Result{Status: StatusFail, Reason: "boom", Exception: &Exception{
			Type: "panic", Message: "boom", Stacktrace: "suite_test.go:42",
		}}
	})
	c.Shutdown()

	if n := len(r.TestSpans()); n != 4 {
		t.Fatalf("got %d test spans, want 4", n)
	}
	children := r.SpansOn(streamChild)
	if len(children) != 2 {
		t.Fatalf("child stream = %v, want only the passing and failing populated bodies", r.ChildNames())
	}
	var sawFailed, sawPopulated bool
	for _, s := range children {
		if s.Name != bodyPhaseName {
			t.Errorf("filter should have dropped %q", s.Name)
		}
		if s.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			sawFailed = true
			parent := otlptest.FindSpan(t, r.TestSpans(), "failing with a child")
			if hexOf(s.ParentSpanId) != hexOf(parent.SpanId) || s.Status.GetMessage() != "boom" {
				t.Errorf("failed populated phase lost its parent or failure: %v", s)
			}
			if len(s.Events) != 1 || s.Events[0].Name != "exception" {
				t.Fatalf("failed populated phase events = %v", s.Events)
			}
			if got := otlptest.StringAttr(t, s.Events[0].Attributes, "exception.message"); got != "boom" {
				t.Errorf("phase exception message = %q", got)
			}
			if got := otlptest.StringAttr(t, s.Events[0].Attributes, "exception.stacktrace"); got != "suite_test.go:42" {
				t.Errorf("phase exception stacktrace = %q", got)
			}
		} else {
			sawPopulated = true
		}
	}
	if !sawFailed || !sawPopulated {
		t.Errorf("expected failed and unset populated phases, got %v", children)
	}
}

func TestFilterRunsOutsideTheLockAndNeverSeesTestSpans(t *testing.T) {
	r := otlptest.NewReceiver(t)
	var seen []string
	var mu sync.Mutex
	c := configured(t, r, nil, withFilter(func(s sdktrace.ReadOnlySpan) bool {
		mu.Lock()
		seen = append(seen, s.Name())
		mu.Unlock()
		// Ending a span from inside the filter must not deadlock.
		_, extra := otel.Tracer("suite").Start(context.Background(), "started-in-filter")
		extra.End()
		return s.Name() != "drop-me"
	}))
	tracer := otel.Tracer("suite")

	run(c, "TestFilter", passes(func(ex *Execution) {
		_, keep := tracer.Start(ex.Context(), "keep-me")
		keep.End()
		_, drop := tracer.Start(ex.Context(), "drop-me")
		drop.End()
	}))
	c.Shutdown()

	got := r.ChildNames()
	if got["keep-me"] != 1 || got["drop-me"] != 0 || got[bodyPhaseName] != 1 {
		t.Fatalf("child stream = %v", got)
	}
	for _, name := range seen {
		if name == testSpanName || isPhaseName(name) {
			t.Errorf("filter saw %q", name)
		}
	}
	if len(seen) != 2 {
		t.Errorf("filter saw %v, want keep-me and drop-me", seen)
	}
}

func TestSpansEndedByTheFilterAreRetainedWithoutConsultingIt(t *testing.T) {
	r := otlptest.NewReceiver(t)
	var calls atomic.Int32
	var held trace.Span
	c := configured(t, r, nil, withFilter(func(s sdktrace.ReadOnlySpan) bool {
		calls.Add(1)
		if held != nil {
			h := held
			held = nil
			h.End()
		}
		return false
	}))
	tracer := otel.Tracer("suite")

	run(c, "TestReentrant", passes(func(ex *Execution) {
		_, h := tracer.Start(ex.Context(), "ended-by-filter")
		held = h
		_, trigger := tracer.Start(ex.Context(), "trigger")
		trigger.End()
	}))
	c.Shutdown()

	got := r.ChildNames()
	if got["ended-by-filter"] != 1 {
		t.Errorf("the span ended by the filter must be retained: %v", got)
	}
	if got["trigger"] != 0 {
		t.Errorf("trigger should have been dropped by the filter: %v", got)
	}
	if calls.Load() != 1 {
		t.Errorf("filter called %d times, want once", calls.Load())
	}
}

func TestFilterPanicRetainsTheSpanAndWarnsOnce(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil, withFilter(func(sdktrace.ReadOnlySpan) bool { panic("filter bug") }))
	tracer := otel.Tracer("suite")

	run(c, "TestPanicFilter", passes(func(ex *Execution) {
		for _, name := range []string{"a", "b"} {
			_, s := tracer.Start(ex.Context(), name)
			s.End()
		}
	}))
	c.Shutdown()

	got := r.ChildNames()
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("child stream = %v, want both spans retained", got)
	}
	want := "Could not filter OpenTelemetry child span, retaining it: filter bug. Further filter failures will not be reported."
	if n := strings.Count(out.String(), want); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, out.String())
	}
	if len(r.TestSpans()) != 1 {
		t.Fatal("test span lost")
	}
}

func TestForwarderIsInertAfterShutdownWithoutShuttingDownTheChildProcessor(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := configured(t, r, nil)
	tracer := otel.Tracer("suite")

	ex := c.Start(Identity{Scope: fixtureScope, Name: "TestInert"}, time.Now())
	ctx := ex.Context()
	_, before := tracer.Start(ctx, "before-shutdown")
	before.End()

	if err := c.forwarder.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, after := tracer.Start(ctx, "after-shutdown")
	after.End()
	if err := c.forwarder.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex.Finish(time.Now(), passed)

	// The child processor is still running: spans already handed to it export.
	if err := c.childProcessor.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Shutdown()

	got := r.ChildNames()
	if got["before-shutdown"] != 1 {
		t.Errorf("span forwarded before shutdown was lost: %v", got)
	}
	if got["after-shutdown"] != 0 || got[bodyPhaseName] != 0 {
		t.Errorf("inert forwarder still forwarded: %v", got)
	}
}

func TestChildQueueOverflowKeepsTheTestSpanAndWarnsBestEffort(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	release := make(chan struct{})
	r.BlockUntil = release
	c := configured(t, r, map[string]string{
		"BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE": "1",
		"BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE": "1",
	})
	tracer := otel.Tracer("suite")

	run(c, "TestChildOverflow", passes(func(ex *Execution) {
		for i := 0; i < 50; i++ {
			_, s := tracer.Start(ex.Context(), "noisy")
			s.End()
		}
	}))
	close(release)
	c.Shutdown()

	if len(r.TestSpans()) != 1 {
		t.Fatalf("test span lost: %d", len(r.TestSpans()))
	}
	if strings.Contains(out.String(), "TEST RESULTS MISSING") {
		t.Fatalf("child drops must not be reported as missing results:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "child span(s) (not-exported: ") || !strings.Contains(out.String(), "test.execution results are unaffected.") {
		t.Fatalf("missing best-effort child warning:\n%s", out.String())
	}
}

func TestSuiteOwnedProviderIsAttachedNotReplaced(t *testing.T) {
	r := otlptest.NewReceiver(t)
	own := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	t.Cleanup(func() { own.Shutdown(context.Background()) })

	t.Run("via WithTracerProvider", func(t *testing.T) {
		c := configured(t, r, nil, withTracerProvider(own))
		if otel.GetTracerProvider() == trace.TracerProvider(own) {
			t.Fatal("the suite's provider must not be installed globally by the collector")
		}
		if !isUnconfiguredGlobal(otel.GetTracerProvider()) {
			t.Fatalf("global provider was replaced: %T", otel.GetTracerProvider())
		}
		run(c, "TestOwnSampler", passes(func(ex *Execution) {
			_, s := own.Tracer("suite").Start(ex.Context(), "sampled-out")
			s.End()
		}))
		c.Shutdown()
		if len(r.TestSpans()) != 1 {
			t.Fatal("test span must export regardless of the suite's sampler")
		}
		if n := len(r.SpansOn(streamChild)); n != 0 {
			t.Fatalf("the suite's never-sample sampler must apply to children, got %d", n)
		}
	})

	t.Run("via the global provider", func(t *testing.T) {
		r := otlptest.NewReceiver(t)
		always := sdktrace.NewTracerProvider()
		t.Cleanup(func() { always.Shutdown(context.Background()) })
		restoreGlobalProvider(t)
		otel.SetTracerProvider(always)
		c := configured(t, r, nil)
		if otel.GetTracerProvider() != trace.TracerProvider(always) {
			t.Fatalf("global provider replaced with %T", otel.GetTracerProvider())
		}
		run(c, "TestGlobalSDK", passes(func(ex *Execution) {
			_, s := otel.Tracer("suite").Start(ex.Context(), "via-global")
			s.End()
		}))
		c.Shutdown()
		if got := r.ChildNames(); got["via-global"] != 1 {
			t.Fatalf("forwarder not attached to the suite's global provider: %v", got)
		}
	})

	// The library sets the collector up from inside the first test, which
	// may have installed a provider of its own by then; the one the suite
	// started with is the one that gets the forwarder.
	t.Run("via the global provider the suite started with", func(t *testing.T) {
		r := otlptest.NewReceiver(t)
		suites, tests := sdktrace.NewTracerProvider(), sdktrace.NewTracerProvider()
		t.Cleanup(func() { suites.Shutdown(context.Background()); tests.Shutdown(context.Background()) })
		restoreGlobalProvider(t)
		otel.SetTracerProvider(tests)
		c := configured(t, r, nil, func(o *Options) { o.GlobalTracerProvider = suites })
		run(c, "TestSnapshot", passes(func(ex *Execution) {
			_, s := suites.Tracer("suite").Start(ex.Context(), "from-the-suite")
			s.End()
			_, s = tests.Tracer("test").Start(ex.Context(), "from-the-test")
			s.End()
		}))
		c.Shutdown()
		if got := r.ChildNames(); got["from-the-suite"] != 1 || got["from-the-test"] != 0 {
			t.Fatalf("child stream = %v, want only the span from the suite's provider", got)
		}
		children := map[string]*tracepb.Span{}
		for _, s := range r.SpansOn(streamChild) {
			children[s.Name] = s
		}
		body := children[bodyPhaseName]
		if body == nil {
			t.Fatalf("child stream = %v, want the %s phase from the suite's provider", r.ChildNames(), bodyPhaseName)
		}
		if hexOf(body.ParentSpanId) != hexOf(r.TestSpans()[0].SpanId) {
			t.Errorf("body phase parent = %x, want the test span %x", body.ParentSpanId, r.TestSpans()[0].SpanId)
		}
		if suite := children["from-the-suite"]; hexOf(suite.ParentSpanId) != hexOf(body.SpanId) {
			t.Errorf("from-the-suite parent = %x, want the exported body phase %x", suite.ParentSpanId, body.SpanId)
		}
	})
}

// When the suite started without a provider and the first test to call
// Start installed one of its own before doing so, that provider stays
// global and sees only the test's spans; the empty phase comes from a
// provider the collector keeps to itself and is not forwarded.
func TestProviderATestInstalledBeforeTheFirstStartIsLeftAlone(t *testing.T) {
	r := otlptest.NewReceiver(t)
	restoreGlobalProvider(t)
	suites := otel.GetTracerProvider()
	if !isUnconfiguredGlobal(suites) {
		t.Skipf("global provider already configured in this process: %T", suites)
	}
	recorder := tracetest.NewSpanRecorder()
	tests := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { tests.Shutdown(context.Background()) })
	otel.SetTracerProvider(tests)

	c := configured(t, r, nil, func(o *Options) { o.GlobalTracerProvider = suites })
	if otel.GetTracerProvider() != trace.TracerProvider(tests) {
		t.Fatalf("the test's provider was replaced with %T", otel.GetTracerProvider())
	}
	run(c, "TestOwnProvider", fails(func(ex *Execution) {
		_, s := otel.Tracer("test").Start(ex.Context(), "from-the-test")
		s.End()
	}))
	c.Shutdown()

	var seen []string
	for _, s := range recorder.Ended() {
		seen = append(seen, s.Name())
	}
	if len(seen) != 1 || seen[0] != "from-the-test" {
		t.Errorf("the test's provider saw %v, want only its own span", seen)
	}
	if got := r.ChildNames(); len(got) != 0 {
		t.Errorf("child stream = %v, want neither the empty phase nor the test's own span", got)
	}
}

func TestCollectorManagedProviderIsInstalledOnlyWhenNobodyConfiguredOne(t *testing.T) {
	r := otlptest.NewReceiver(t)
	restoreGlobalProvider(t)
	if !isUnconfiguredGlobal(otel.GetTracerProvider()) {
		t.Skipf("global provider already configured in this process: %T", otel.GetTracerProvider())
	}
	c := configured(t, r, nil)
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("expected a collector-managed SDK provider, got %T", otel.GetTracerProvider())
	}
	run(c, "TestManaged", passes(func(ex *Execution) {
		_, s := otel.Tracer("suite").Start(ex.Context(), "managed")
		s.End()
	}))
	c.Shutdown()
	if got := r.ChildNames(); got["managed"] != 1 {
		t.Fatalf("child stream = %v", got)
	}
}

// unregistrable is a provider that cannot take span processors, such as a
// vendor's own implementation of the API.
type unregistrable struct{ trace.TracerProvider }

func TestUnknownGlobalProviderKeepsTestSpansOnly(t *testing.T) {
	out := captureWarnings(t)
	r := otlptest.NewReceiver(t)
	restoreGlobalProvider(t)
	recorder := tracetest.NewSpanRecorder()
	sdk := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { sdk.Shutdown(context.Background()) })
	custom := unregistrable{sdk}
	otel.SetTracerProvider(custom)

	c := configured(t, r, nil)
	if otel.GetTracerProvider() != trace.TracerProvider(custom) {
		t.Fatalf("unknown provider was replaced with %T", otel.GetTracerProvider())
	}
	if c.forwarder != nil || c.childProcessor != nil {
		t.Fatal("child export should be disabled")
	}
	if !strings.Contains(out.String(), "OpenTelemetry child span export disabled: the tracer provider collector.unregistrable cannot register span processors") || !strings.Contains(out.String(), "test.execution export remains enabled") {
		t.Fatalf("warning = %q", out.String())
	}
	run(c, "TestNoChildren", fails(func(*Execution) {}))
	c.Shutdown()
	if len(r.TestSpans()) != 1 {
		t.Fatal("test span must still export")
	}
	if n := len(r.Stream(streamChild)); n != 0 {
		t.Fatalf("%d child requests sent with child export disabled", n)
	}
	if n := len(recorder.Ended()); n != 0 {
		t.Fatalf("the collector started %d span(s) on a provider it could not attach to", n)
	}
}
