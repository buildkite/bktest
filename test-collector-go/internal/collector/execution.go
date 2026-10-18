package collector

import (
	"context"
	"maps"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// Identity is what Test Engine uses to recognise a test across runs: Scope
// is the package import path, Name is the test name as t.Name() reports it,
// and File and Line locate the test function relative to the module root.
// File may be empty when the location is unknown.
type Identity struct {
	Scope string
	Name  string
	File  string
	Line  int
}

// Status is a test execution's result as Test Engine understands it. The
// values are the test.case.result.status attribute; "skipped" rather than
// "skip" is what the receiver matches.
type Status string

const (
	StatusPass    Status = "pass"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"

	statusUnset = "unset"
)

// Result is how an execution ended. Reason is the failure reason shown in
// Test Engine, capped at 1,024 characters; Exception, when set, carries the
// failure detail as an exception event.
type Result struct {
	Status    Status
	Reason    string
	Exception *Exception
}

// Exception is one failure's detail. Message is capped at 10,243 characters
// and Stacktrace at 16,384; an empty field is omitted from the event.
type Exception struct {
	Type       string
	Message    string
	Stacktrace string
}

// FailureReasonLength is the longest failure reason Test Engine keeps.
const FailureReasonLength = 1024

type executionKey struct{}

// Execution is one test in flight: its root span on the private provider
// and, when child spans are enabled, its test.body phase span on the
// provider carrying the forwarder.
type Execution struct {
	c        *Collector
	span     trace.Span
	body     trace.Span
	traceID  trace.TraceID
	start    time.Time
	identity Identity
	external string
	ctx      context.Context

	mu   sync.Mutex
	tags map[string]string
}

// ExecutionFromContext returns the execution a context from
// Execution.Context carries, or nil.
func ExecutionFromContext(ctx context.Context) *Execution {
	if ctx == nil {
		return nil
	}
	ex, _ := ctx.Value(executionKey{}).(*Execution)
	return ex
}

// Start opens the spans for one test execution starting at start. A failure
// to start is warned about, counted as a dropped result and returns nil.
func (c *Collector) Start(id Identity, start time.Time) (ex *Execution) {
	defer func() {
		if r := recover(); r != nil {
			c.testStats.startFailure()
			Warnf("TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (could not start span: %v).\n%s", r, debug.Stack())
			Warnf("OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.")
			ex = nil
		}
	}()

	id.Scope = strings.ToValidUTF8(id.Scope, "\uFFFD")
	id.Name = strings.ToValidUTF8(id.Name, "\uFFFD")
	id.File = strings.ToValidUTF8(id.File, "\uFFFD")
	ex = &Execution{c: c, start: start, identity: id, external: uuid.Must(uuid.NewV7()).String(), tags: map[string]string{}}

	startAttrs := []attribute.KeyValue{
		attribute.String(attrExecutionVia, executionVia),
		attribute.String(attrRunKey, c.env.key),
		semconv.TestCaseResultStatusKey.String(statusUnset),
	}
	startAttrs = append(startAttrs, ex.testAttributes()...)

	rootCtx, span := c.testTracer.Start(context.Background(), testSpanName,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(start),
		trace.WithLinks(c.links...),
		trace.WithAttributes(startAttrs...),
	)
	ex.span = span
	ex.traceID = span.SpanContext().TraceID()
	ex.ctx = context.WithValue(rootCtx, executionKey{}, ex)

	if c.childSpans {
		// The body phase lives on the suite's provider, which the collector
		// does not control; if it cannot start one the test still has its
		// result.
		attempt("Could not start the OpenTelemetry test.body phase span", func() {
			ex.ctx, ex.body = c.phaseTracer().Start(ex.ctx, bodyPhaseName, trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(start))
		})
	}
	return ex
}

// Context carries the execution and, when child spans are enabled, makes
// its test.body phase the current span, so instrumentation started from it
// nests under the test.
func (ex *Execution) Context() context.Context { return ex.ctx }

// phaseTracer comes from the provider that carries the forwarder, so phase
// and instrumentation spans share one pipeline.
func (c *Collector) phaseTracer() trace.Tracer {
	tp := c.childProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(tracerName, trace.WithInstrumentationVersion(Version))
}

// Finish records the result on the body and test spans and ends them at
// end. Every step recovers on its own: a half-described execution beats a
// missing one.
func (ex *Execution) Finish(end time.Time, r Result) {
	defer func() {
		if rec := recover(); rec != nil {
			Warnf("Could not finish the OpenTelemetry test span: %v\n%s", rec, debug.Stack())
		}
	}()

	recordFailure := func(span trace.Span) {
		if r.Status != StatusFail {
			return
		}
		span.SetStatus(codes.Error, truncate(r.Reason, FailureReasonLength))
		if e := r.Exception; e != nil {
			attrs := []attribute.KeyValue{semconv.ExceptionType(e.Type)}
			if e.Message != "" {
				attrs = append(attrs, semconv.ExceptionMessage(truncate(e.Message, exceptionMessageLength)))
			}
			if e.Stacktrace != "" {
				attrs = append(attrs, semconv.ExceptionStacktrace(truncate(e.Stacktrace, testSpanEventAttributeLength)))
			}
			span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(attrs...))
		}
	}

	if ex.body != nil {
		attempt("Could not finish OpenTelemetry phase span", func() {
			recordFailure(ex.body)
			ex.endSpan(ex.body, end)
		})
	}
	attempt("Could not record the OpenTelemetry test result", func() {
		ex.span.SetAttributes(semconv.TestCaseResultStatusKey.String(string(r.Status)))
		recordFailure(ex.span)
	})
	attempt("Could not describe OpenTelemetry test span", func() {
		ex.span.SetAttributes(ex.testAttributes()...)
		ex.span.SetAttributes(ex.c.runAttrs...)
		ex.span.SetAttributes(tagAttributes(ex.mergedTags())...)
	})
	attempt("Could not finish OpenTelemetry test span", func() {
		ex.c.awaitRoom()
		ex.endSpan(ex.span, end)
	})
}

// endSpan applies the captured end time unless the clock stepped backwards,
// in which case the SDK's own clock wins.
func (ex *Execution) endSpan(span trace.Span, end time.Time) {
	if end.Before(ex.start) {
		span.End()
		return
	}
	span.End(trace.WithTimestamp(end))
}

func attempt(failure string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			Warnf("%s: %v", failure, r)
		}
	}()
	fn()
}

// Tag sets a per-execution tag, overriding a run-level tag of the same key.
func (ex *Execution) Tag(key, value string) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	ex.tags[key] = strings.ToValidUTF8(value, "\uFFFD")
}

// mergedTags returns run-level tags with per-test tags overriding them.
func (ex *Execution) mergedTags() map[string]string {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	merged := make(map[string]string, len(ex.c.tags)+len(ex.tags))
	maps.Copy(merged, ex.c.tags)
	maps.Copy(merged, ex.tags)
	return merged
}

// Annotate adds a test.annotation event to the test span.
func (ex *Execution) Annotate(content string) {
	// The SDK's attribute length limit does not apply to event attributes,
	// and the protobuf encoder rejects a batch holding invalid UTF-8.
	content = strings.ToValidUTF8(content, "\uFFFD")
	ex.span.AddEvent(annotationEventName, trace.WithAttributes(attribute.String(attrAnnotation, truncate(content, testSpanEventAttributeLength))))
}

// testAttributes are the identity attributes, without tags.
func (ex *Execution) testAttributes() []attribute.KeyValue {
	id := ex.identity
	attrs := []attribute.KeyValue{
		attribute.String(attrTestScope, id.Scope),
		attribute.String(attrTestName, id.Name),
		semconv.TestCaseName(id.Scope + "." + id.Name),
		semconv.TestSuiteName(id.Scope),
	}
	if id.File != "" {
		attrs = append(attrs, semconv.CodeFilePath(ex.c.locationPrefix+id.File))
		if id.Line > 0 {
			attrs = append(attrs, semconv.CodeLineNumber(id.Line))
		}
	}
	return append(attrs, attribute.String(attrExternalID, ex.external))
}

// truncate limits s to limit runes, marking the cut with "...".
func truncate(s string, limit int) string {
	if limit <= 3 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-3]) + "..."
}
