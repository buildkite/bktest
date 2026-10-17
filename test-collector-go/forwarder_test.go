package bktest

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func childNames(r *receiver) map[string]int {
	names := map[string]int{}
	for _, s := range r.spansOn(streamChild) {
		names[s.Name]++
	}
	return names
}

func TestForwardsOnlySpansFromTheExecutionTrace(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, nil)
	tracer := otel.Tracer("suite")

	runTest(c, "TestChildren", func(ctx context.Context, ft *fakeT) {
		ctx, parent := tracer.Start(ctx, "parent")
		_, grandchild := tracer.Start(ctx, "grandchild")
		grandchild.End()
		parent.End()

		_, outside := tracer.Start(context.Background(), "outside-the-test")
		outside.End()

		// Same context key, different trace: a customer root span inside a test.
		_, otherRoot := tracer.Start(ctx, "other-root", trace.WithNewRoot())
		otherRoot.End()
	})
	c.shutdown()

	got := childNames(r)
	want := map[string]int{"parent": 1, "grandchild": 1, bodyPhaseName: 1}
	if len(got) != len(want) {
		t.Fatalf("child stream = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("child stream %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	for _, s := range r.spansOn(streamChild) {
		if s.Name == bodyPhaseName && hexOf(s.ParentSpanId) != hexOf(r.testSpans()[0].SpanId) {
			t.Errorf("body phase parent = %x, want the test span %x", s.ParentSpanId, r.testSpans()[0].SpanId)
		}
		if s.Name == "parent" && hexOf(s.ParentSpanId) == hexOf(r.testSpans()[0].SpanId) {
			t.Errorf("instrumentation should nest under the body phase, not directly under the test span")
		}
	}
}

func TestPhaseExportRule(t *testing.T) {
	r := newReceiver(t)
	dropAll := func(sdktrace.ReadOnlySpan) bool { return false }
	c := configured(t, r, nil, WithSpanFilter(dropAll))
	tracer := otel.Tracer("suite")

	runTest(c, "passing with nothing", nil)
	runTest(c, "failing with nothing", func(_ context.Context, ft *fakeT) { ft.Fail() })
	runTest(c, "passing with a child", func(ctx context.Context, ft *fakeT) {
		_, child := tracer.Start(ctx, "child")
		child.End()
	})
	c.shutdown()

	if n := len(r.testSpans()); n != 3 {
		t.Fatalf("got %d test spans, want 3", n)
	}
	children := r.spansOn(streamChild)
	if len(children) != 2 {
		t.Fatalf("child stream = %v, want one failed empty body and one populated body", childNames(r))
	}
	var sawFailed, sawPopulated bool
	for _, s := range children {
		if s.Name != bodyPhaseName {
			t.Errorf("filter should have dropped %q", s.Name)
		}
		if s.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			sawFailed = true
		} else {
			sawPopulated = true
		}
	}
	if !sawFailed || !sawPopulated {
		t.Errorf("expected a failed empty phase and a populated unset phase, got %v", children)
	}
}

func TestFilterRunsOutsideTheLockAndNeverSeesTestSpans(t *testing.T) {
	r := newReceiver(t)
	var seen []string
	var mu sync.Mutex
	c := configured(t, r, nil, WithSpanFilter(func(s sdktrace.ReadOnlySpan) bool {
		mu.Lock()
		seen = append(seen, s.Name())
		mu.Unlock()
		// Ending a span from inside the filter must not deadlock.
		_, extra := otel.Tracer("suite").Start(context.Background(), "started-in-filter")
		extra.End()
		return s.Name() != "drop-me"
	}))
	tracer := otel.Tracer("suite")

	runTest(c, "TestFilter", func(ctx context.Context, ft *fakeT) {
		_, keep := tracer.Start(ctx, "keep-me")
		keep.End()
		_, drop := tracer.Start(ctx, "drop-me")
		drop.End()
	})
	c.shutdown()

	got := childNames(r)
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
	r := newReceiver(t)
	var calls atomic.Int32
	var held trace.Span
	c := configured(t, r, nil, WithSpanFilter(func(s sdktrace.ReadOnlySpan) bool {
		calls.Add(1)
		if held != nil {
			h := held
			held = nil
			h.End()
		}
		return false
	}))
	tracer := otel.Tracer("suite")

	runTest(c, "TestReentrant", func(ctx context.Context, ft *fakeT) {
		_, h := tracer.Start(ctx, "ended-by-filter")
		held = h
		_, trigger := tracer.Start(ctx, "trigger")
		trigger.End()
	})
	c.shutdown()

	got := childNames(r)
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
	r := newReceiver(t)
	c := configured(t, r, nil, WithSpanFilter(func(sdktrace.ReadOnlySpan) bool { panic("filter bug") }))
	tracer := otel.Tracer("suite")

	runTest(c, "TestPanicFilter", func(ctx context.Context, ft *fakeT) {
		for _, name := range []string{"a", "b"} {
			_, s := tracer.Start(ctx, name)
			s.End()
		}
	})
	c.shutdown()

	got := childNames(r)
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("child stream = %v, want both spans retained", got)
	}
	want := "Could not filter OpenTelemetry child span, retaining it: filter bug. Further filter failures will not be reported."
	if n := strings.Count(out.String(), want); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, out.String())
	}
	if len(r.testSpans()) != 1 {
		t.Fatal("test span lost")
	}
}

func TestForwarderIsInertAfterShutdownWithoutShuttingDownTheChildProcessor(t *testing.T) {
	r := newReceiver(t)
	c := configured(t, r, nil)
	tracer := otel.Tracer("suite")

	ft := newFakeT("TestInert")
	ctx := c.startTest(ft)
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
	ft.finish()

	// The child processor is still running: spans already handed to it export.
	if err := c.childProcessor.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.shutdown()

	got := childNames(r)
	if got["before-shutdown"] != 1 {
		t.Errorf("span forwarded before shutdown was lost: %v", got)
	}
	if got["after-shutdown"] != 0 || got[bodyPhaseName] != 0 {
		t.Errorf("inert forwarder still forwarded: %v", got)
	}
}

func TestChildQueueOverflowKeepsTheTestSpanAndWarnsBestEffort(t *testing.T) {
	out := captureWarnings(t)
	r := newReceiver(t)
	release := make(chan struct{})
	r.blockUntil = release
	c := configured(t, r, map[string]string{
		"BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE": "1",
		"BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE": "1",
	})
	tracer := otel.Tracer("suite")

	runTest(c, "TestChildOverflow", func(ctx context.Context, ft *fakeT) {
		for i := 0; i < 50; i++ {
			_, s := tracer.Start(ctx, "noisy")
			s.End()
		}
	})
	close(release)
	c.shutdown()

	if len(r.testSpans()) != 1 {
		t.Fatalf("test span lost: %d", len(r.testSpans()))
	}
	if strings.Contains(out.String(), "TEST RESULTS MISSING") {
		t.Fatalf("child drops must not be reported as missing results:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "child span(s) (not-exported); test.execution results are unaffected.") {
		t.Fatalf("missing best-effort child warning:\n%s", out.String())
	}
}

func TestSuiteOwnedProviderIsAttachedNotReplaced(t *testing.T) {
	r := newReceiver(t)
	own := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	t.Cleanup(func() { own.Shutdown(context.Background()) })

	t.Run("via WithTracerProvider", func(t *testing.T) {
		c := configured(t, r, nil, WithTracerProvider(own))
		if otel.GetTracerProvider() == trace.TracerProvider(own) {
			t.Fatal("the suite's provider must not be installed globally by the collector")
		}
		if !isUnconfiguredGlobal(otel.GetTracerProvider()) {
			t.Fatalf("global provider was replaced: %T", otel.GetTracerProvider())
		}
		runTest(c, "TestOwnSampler", func(ctx context.Context, ft *fakeT) {
			_, s := own.Tracer("suite").Start(ctx, "sampled-out")
			s.End()
		})
		c.shutdown()
		if len(r.testSpans()) != 1 {
			t.Fatal("test span must export regardless of the suite's sampler")
		}
		if n := len(r.spansOn(streamChild)); n != 0 {
			t.Fatalf("the suite's never-sample sampler must apply to children, got %d", n)
		}
	})

	t.Run("via the global provider", func(t *testing.T) {
		r := newReceiver(t)
		always := sdktrace.NewTracerProvider()
		t.Cleanup(func() { always.Shutdown(context.Background()) })
		restoreGlobalProvider(t)
		otel.SetTracerProvider(always)
		c := configured(t, r, nil)
		if otel.GetTracerProvider() != trace.TracerProvider(always) {
			t.Fatalf("global provider replaced with %T", otel.GetTracerProvider())
		}
		runTest(c, "TestGlobalSDK", func(ctx context.Context, ft *fakeT) {
			_, s := otel.Tracer("suite").Start(ctx, "via-global")
			s.End()
		})
		c.shutdown()
		if got := childNames(r); got["via-global"] != 1 {
			t.Fatalf("forwarder not attached to the suite's global provider: %v", got)
		}
	})
}

func TestCollectorManagedProviderIsInstalledOnlyWhenNobodyConfiguredOne(t *testing.T) {
	r := newReceiver(t)
	restoreGlobalProvider(t)
	if !isUnconfiguredGlobal(otel.GetTracerProvider()) {
		t.Skipf("global provider already configured in this process: %T", otel.GetTracerProvider())
	}
	c := configured(t, r, nil)
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("expected a collector-managed SDK provider, got %T", otel.GetTracerProvider())
	}
	runTest(c, "TestManaged", func(ctx context.Context, ft *fakeT) {
		_, s := otel.Tracer("suite").Start(ctx, "managed")
		s.End()
	})
	c.shutdown()
	if got := childNames(r); got["managed"] != 1 {
		t.Fatalf("child stream = %v", got)
	}
}

func TestUnknownGlobalProviderKeepsTestSpansOnly(t *testing.T) {
	out := captureWarnings(t)
	r := newReceiver(t)
	restoreGlobalProvider(t)
	custom := noop.NewTracerProvider()
	otel.SetTracerProvider(custom)

	c := configured(t, r, nil)
	if otel.GetTracerProvider() != trace.TracerProvider(custom) {
		t.Fatalf("unknown provider was replaced with %T", otel.GetTracerProvider())
	}
	if c.forwarder != nil || c.childProcessor != nil {
		t.Fatal("child export should be disabled")
	}
	if !strings.Contains(out.String(), "OpenTelemetry child span export disabled: the tracer provider noop.TracerProvider cannot register span processors") || !strings.Contains(out.String(), "test.execution export remains enabled") {
		t.Fatalf("warning = %q", out.String())
	}
	runTest(c, "TestNoChildren", func(ctx context.Context, ft *fakeT) { ft.Fail() })
	c.shutdown()
	if len(r.testSpans()) != 1 {
		t.Fatal("test span must still export")
	}
	if n := len(r.stream(streamChild)); n != 0 {
		t.Fatalf("%d child requests sent with child export disabled", n)
	}
}
