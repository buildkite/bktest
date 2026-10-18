package collector

import (
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// forwarder is the span processor registered on whichever provider owns the
// suite's spans (see WithTracerProvider). It passes spans that belong to a running test's trace to the child stream and
// ignores everything else the suite's instrumentation produces.
type forwarder struct {
	next   sdktrace.SpanProcessor
	filter func(sdktrace.ReadOnlySpan) bool

	mu      sync.Mutex
	active  bool
	tracked map[trace.SpanID]*trackedSpan

	filterWarned sync.Once
}

type trackedSpan struct {
	phase     bool
	populated bool
}

func newForwarder(next sdktrace.SpanProcessor, filter func(sdktrace.ReadOnlySpan) bool) *forwarder {
	return &forwarder{next: next, filter: filter, active: true, tracked: map[trace.SpanID]*trackedSpan{}}
}

func isPhaseName(name string) bool {
	return name == setupPhaseName || name == bodyPhaseName || name == teardownPhaseName
}

// OnStart tracks a span whose parent context belongs to a test and whose
// trace is that test's. A tracked non-phase span marks its parent phase as
// populated, before any filter gets a say.
func (f *forwarder) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	defer func() {
		if r := recover(); r != nil {
			Warnf("Could not track OpenTelemetry child span: %v", r)
		}
	}()
	ex := ExecutionFromContext(parent)
	if ex == nil || s.SpanContext().TraceID() != ex.traceID {
		return
	}
	phase := isPhaseName(s.Name())

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return
	}
	f.tracked[s.SpanContext().SpanID()] = &trackedSpan{phase: phase}
	if !phase {
		if p, ok := f.tracked[s.Parent().SpanID()]; ok && p.phase {
			p.populated = true
		}
	}
}

// OnEnd forwards a tracked span to the child stream: phases when populated
// or not UNSET, other spans when the filter accepts them.
func (f *forwarder) OnEnd(s sdktrace.ReadOnlySpan) {
	defer func() {
		if r := recover(); r != nil {
			Warnf("Could not export OpenTelemetry child span: %v", r)
		}
	}()

	f.mu.Lock()
	ts, ok := f.tracked[s.SpanContext().SpanID()]
	if ok {
		delete(f.tracked, s.SpanContext().SpanID())
	}
	f.mu.Unlock()
	if !ok {
		return
	}
	if ts.phase {
		if ts.populated || s.Status().Code != codes.Unset {
			f.forward(s)
		}
		return
	}
	if f.filter != nil && !f.keep(s) {
		return
	}
	f.forward(s)
}

// forward hands s to the child stream unless the forwarder has been
// deactivated; one lock covers both so a shutdown cannot slip in between.
func (f *forwarder) forward(s sdktrace.ReadOnlySpan) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active {
		f.next.OnEnd(s)
	}
}

// keep runs the customer filter without holding the lock. A panicking filter
// retains the span; spans the filter itself ends are retained without
// consulting it, so it cannot recurse.
func (f *forwarder) keep(s sdktrace.ReadOnlySpan) (keep bool) {
	if insideFilter() {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			keep = true
			f.filterWarned.Do(func() {
				Warnf("Could not filter OpenTelemetry child span, retaining it: %v. Further filter failures will not be reported.\n%s", r, debug.Stack())
			})
		}
	}()
	return f.runFilter(s)
}

// runFilter is a named frame that insideFilter looks for on the stack; do
// not inline or rename it.
func (f *forwarder) runFilter(s sdktrace.ReadOnlySpan) bool {
	return f.filter(s)
}

// insideFilter reports whether this goroutine is already running the filter,
// which happens when the filter itself ends a span.
func insideFilter() bool {
	pcs := make([]uintptr, 128)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".(*forwarder).runFilter") {
			return true
		}
		if !more {
			return false
		}
	}
}

// ForceFlush flushes the child stream while the forwarder is active.
func (f *forwarder) ForceFlush(ctx context.Context) error {
	f.mu.Lock()
	active := f.active
	f.mu.Unlock()
	if !active {
		return nil
	}
	return f.next.ForceFlush(ctx)
}

// Shutdown makes the forwarder inert. The child processor is shut down by
// the collector, not here, so a suite that shuts down its own provider does
// not cut off spans still queued for export.
func (f *forwarder) Shutdown(context.Context) error {
	f.deactivate()
	return nil
}

func (f *forwarder) deactivate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = false
	f.tracked = map[trace.SpanID]*trackedSpan{}
}

var _ sdktrace.SpanProcessor = (*forwarder)(nil)
