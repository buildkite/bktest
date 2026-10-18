// Package collector exports test executions to Buildkite Test Engine over
// OTLP. The bktest package feeds it from inside a go test process and the
// buildkite-test-collector command feeds it from go test -json output; both
// produce the same spans.
package collector

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Version is the collector version reported on every test execution.
const Version = "0.1.0"

// Options configure a Collector.
type Options struct {
	// Tags are run-level tags, exported on every execution as buildkite.tag.<key>.
	Tags map[string]string
	// LocationPrefix is prepended to every execution's file path.
	LocationPrefix string
	// ChildSpans enables the child stream: a test.body phase under every
	// execution and a forwarder for the suite's own instrumentation spans.
	ChildSpans bool
	// Filter drops child spans it rejects. It never sees test.execution or
	// phase spans.
	Filter func(sdktrace.ReadOnlySpan) bool
	// TracerProvider is the suite's own provider to attach the forwarder to.
	// Nil means the global provider, installing one when nobody has.
	TracerProvider trace.TracerProvider
}

type spanProcessorRegistrar interface {
	RegisterSpanProcessor(sdktrace.SpanProcessor)
}

// Collector is the configured export pipeline for one run in this process.
type Collector struct {
	env            runEnv
	locationPrefix string
	tags           map[string]string
	runAttrs       []attribute.KeyValue
	links          []trace.Link

	testProvider *sdktrace.TracerProvider
	testTracer   trace.Tracer
	testStats    *streamStats

	childSpans bool
	// childProvider is the suite's own provider from Options.TracerProvider;
	// nil means the global provider at the time each test starts.
	childProvider  trace.TracerProvider
	childProcessor sdktrace.SpanProcessor
	childStats     *streamStats
	forwarder      *forwarder

	shutdownOnce  sync.Once
	abortDeadline func() time.Time
}

// ErrNoToken is the error New returns when no token is configured. Callers
// warn about it once and run without export.
var ErrNoToken = errors.New("no token configured. Set BUILDKITE_TESTS_OTLP_TOKEN or BUILDKITE_ANALYTICS_TOKEN; test results will not be uploaded to Buildkite Test Engine")

// New performs the mandatory test-span setup and, when opts.ChildSpans is
// set, the optional child-span setup. The environment supplies the token,
// run key, endpoint and CI metadata.
func New(ctx context.Context, opts Options) (*Collector, error) {
	token := apiToken()
	env := detectRunEnv()
	if token == "" {
		return nil, ErrNoToken
	}
	if !validRunKey(env.key) {
		// Wording shared with the other collectors.
		return nil, fmt.Errorf("run key %q is invalid. Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); it must be 1-255 printable ASCII characters without spaces", env.key)
	}
	endpointURL, err := validateEndpoint(endpoint())
	if err != nil {
		return nil, err
	}

	c := &Collector{
		env:            env,
		locationPrefix: opts.LocationPrefix,
		childSpans:     opts.ChildSpans,
		childProvider:  opts.TracerProvider,
		tags:           runTags(opts.Tags),
		links:          jobSpanLinks(),
		testStats:      &streamStats{},
		childStats:     &streamStats{},
		abortDeadline:  sync.OnceValue(func() time.Time { return time.Now().Add(processorTimeout) }),
	}
	c.runAttrs = runAttributes(env, opts.LocationPrefix)
	res := producerResource(env)

	testSizes := spanProcessorSizes(defaultTestSpanProcessorEnvName, processorSizes{queue: testSpanQueueSize, batch: testSpanBatchSize})
	testExporter, err := newExporter(ctx, endpointURL, token, env.key, streamTest)
	if err != nil {
		return nil, fmt.Errorf("could not create the test span exporter: %w", err)
	}
	testProcessor := sdktrace.NewBatchSpanProcessor(
		countingExporter{SpanExporter: testExporter, stats: c.testStats},
		sdktrace.WithMaxQueueSize(testSizes.queue),
		sdktrace.WithMaxExportBatchSize(testSizes.batch),
		sdktrace.WithBatchTimeout(testSpanScheduleDelay),
		sdktrace.WithExportTimeout(exportTimeout),
	)
	c.testProvider = sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithIDGenerator(secureIDGenerator{}),
		sdktrace.WithResource(res),
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
			AttributeCountLimit:         testSpanAttributeCountLimit,
			AttributeValueLengthLimit:   testSpanAttributeLengthLimit,
			EventCountLimit:             testSpanEventCountLimit,
			AttributePerEventCountLimit: testSpanEventAttributeCountMax,
			LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
			AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
		}),
		sdktrace.WithSpanProcessor(countingProcessor{SpanProcessor: testProcessor, stats: c.testStats}),
	)
	c.testTracer = c.testProvider.Tracer(tracerName, trace.WithInstrumentationVersion(Version))

	if opts.ChildSpans {
		if err := c.configureChildExport(ctx, opts, endpointURL, token, res); err != nil {
			Warnf("OpenTelemetry child span export disabled: %v; test.execution export remains enabled", err)
		}
	}
	return c, nil
}

// RunKey is the Test Engine run key every execution is reported under.
func (c *Collector) RunKey() string { return c.env.key }

// Exported is the number of test.execution spans the receiver has accepted
// so far; call it after Shutdown for the final count.
func (c *Collector) Exported() int {
	c.testStats.mu.Lock()
	defer c.testStats.mu.Unlock()
	return c.testStats.succeeded
}

// configureChildExport attaches the forwarder to the provider that owns the
// suite's instrumentation spans, per the ownership rules on
// Options.TracerProvider. Child export is optional, so a provider that
// panics only costs child spans.
func (c *Collector) configureChildExport(ctx context.Context, opts Options, endpointURL *url.URL, token string, res *resource.Resource) error {
	childSizes := spanProcessorSizes(childSpanProcessorEnvName, processorSizes{queue: childSpanQueueSize, batch: childSpanBatchSize})
	childExporter, err := newExporter(ctx, endpointURL, token, c.env.key, streamChild)
	if err != nil {
		return fmt.Errorf("could not create the child span exporter: %w", err)
	}
	processor := sdktrace.NewBatchSpanProcessor(
		countingExporter{SpanExporter: childExporter, stats: c.childStats},
		sdktrace.WithMaxQueueSize(childSizes.queue),
		sdktrace.WithMaxExportBatchSize(childSizes.batch),
		sdktrace.WithBatchTimeout(childSpanDelay),
		sdktrace.WithExportTimeout(exportTimeout),
	)
	fwd := newForwarder(countingProcessor{SpanProcessor: processor, stats: c.childStats}, opts.Filter)

	attach := func(tp trace.TracerProvider) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tracer provider %T panicked while registering the span processor: %v", tp, r)
			}
		}()
		registrar, ok := tp.(spanProcessorRegistrar)
		if !ok {
			return fmt.Errorf("the tracer provider %T cannot register span processors; pass an SDK provider with bktest.WithTracerProvider", tp)
		}
		registrar.RegisterSpanProcessor(fwd)
		return nil
	}

	tp := opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
		if isUnconfiguredGlobal(tp) {
			otel.SetTracerProvider(sdktrace.NewTracerProvider(
				sdktrace.WithResource(res),
				sdktrace.WithIDGenerator(secureIDGenerator{}),
				sdktrace.WithSpanProcessor(fwd),
			))
			tp = nil
		}
	}
	var attachErr error
	if tp != nil {
		attachErr = attach(tp)
	}
	if attachErr != nil {
		fwd.Shutdown(ctx)
		if err := processor.Shutdown(ctx); err != nil {
			Warnf("Could not shut down the child span processor: %v", err)
		}
		return attachErr
	}
	c.childProcessor = processor
	c.forwarder = fwd
	return nil
}

// isUnconfiguredGlobal reports whether tp is the API's delegating default,
// which is what otel.GetTracerProvider returns until something calls
// otel.SetTracerProvider. Any other type is a provider somebody chose.
func isUnconfiguredGlobal(tp trace.TracerProvider) bool {
	typ := reflect.TypeOf(tp)
	if typ == nil {
		return false
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ.PkgPath() == "go.opentelemetry.io/otel/internal/global"
}

// jobSpanLinks links each test span to the Buildkite Agent's job span when
// the agent exported TRACEPARENT/TRACESTATE. A malformed value yields no link.
func jobSpanLinks() []trace.Link {
	carrier := propagation.MapCarrier{}
	for _, key := range []string{"traceparent", "tracestate"} {
		if v := os.Getenv(strings.ToUpper(key)); v != "" {
			carrier.Set(key, v)
		}
	}
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
	if !sc.IsValid() {
		return nil
	}
	return []trace.Link{{SpanContext: sc}}
}

// Shutdown deactivates the forwarder so no new child spans enqueue, then
// shuts down the test stream and the child stream against one shared 30 s
// deadline and reports anything that was lost. It is safe to call more
// than once.
func (c *Collector) Shutdown() {
	c.shutdownOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), processorTimeout)
		defer cancel()
		if c.forwarder != nil {
			c.forwarder.deactivate()
		}
		testDrained := c.drain(ctx, "shut down OpenTelemetry span export", c.testProvider.Shutdown)
		childDrained := true
		if c.childProcessor != nil {
			childDrained = c.drain(ctx, "shut down OpenTelemetry child span export", c.childProcessor.Shutdown)
		}
		c.reportDrops(testDrained, childDrained)
	})
}

// Abort flushes both streams while a panic unwinds the test binary. It is
// a flush, not a shutdown, because go test runs the cleanups of the
// panicking test's ancestors afterwards and their results still need a
// working pipeline. Every abort shares the deadline the first one set.
func (c *Collector) Abort() {
	ctx, cancel := context.WithDeadline(context.Background(), c.abortDeadline())
	defer cancel()
	testDrained := c.drain(ctx, "flush OpenTelemetry span export", c.testProvider.ForceFlush)
	childDrained := true
	if c.childProcessor != nil {
		childDrained = c.drain(ctx, "flush OpenTelemetry child span export", c.childProcessor.ForceFlush)
	}
	c.reportDrops(testDrained, childDrained)
}

// drain runs a flush or shutdown and reports whether it finished before the
// deadline, so the caller knows whether unexported spans are lost or may
// still be in flight.
func (c *Collector) drain(ctx context.Context, what string, op func(context.Context) error) bool {
	err := op(ctx)
	if err == nil {
		return true
	}
	Warnf("Could not %s: %v", what, err)
	return !errors.Is(err, context.DeadlineExceeded)
}

// reportDrops warns about test spans that were lost (TEST RESULTS MISSING)
// and about child spans that were lost (best effort). Drops are reported
// once per increase; spans still in flight at the deadline are reported as
// pending each time.
func (c *Collector) reportDrops(testDrained, childDrained bool) {
	if test, unreported := c.testStats.unreported(testDrained); unreported {
		Warnf("TEST RESULTS MISSING: OpenTelemetry dropped %d test.execution span(s) (%s).", test.dropped, test.reason)
		Warnf("OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.")
	} else if test.pending > 0 {
		Warnf("TEST RESULTS MISSING: %d test.execution span(s) were still exporting when the %s deadline passed and may not have reached Buildkite Test Engine.", test.pending, processorTimeout)
	}

	if child, unreported := c.childStats.unreported(childDrained); unreported {
		Warnf("OpenTelemetry dropped %d child span(s) (%s); test.execution results are unaffected.", child.dropped, child.reason)
	} else if child.pending > 0 {
		Warnf("OpenTelemetry child span export did not finish within %s; %d child span(s) may be missing. test.execution results are unaffected.", processorTimeout, child.pending)
	}
}

// secureIDGenerator draws trace and span ids from crypto/rand; the SDK
// default uses math/rand/v2, and Test Engine wants ids that are unguessable
// as well as collision-free.
type secureIDGenerator struct{}

func (secureIDGenerator) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	var tid trace.TraceID
	for !tid.IsValid() {
		mustRandom(tid[:])
	}
	return tid, newSpanID()
}

func (secureIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	return newSpanID()
}

func newSpanID() trace.SpanID {
	var sid trace.SpanID
	for !sid.IsValid() {
		mustRandom(sid[:])
	}
	return sid
}

func mustRandom(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("bktest: crypto/rand unavailable: " + err.Error())
	}
}

var _ sdktrace.IDGenerator = secureIDGenerator{}
