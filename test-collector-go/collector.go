package bktest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Option configures Main.
type Option func(*options)

type options struct {
	tags           map[string]string
	locationPrefix string
	filter         SpanFilter
	tracerProvider trace.TracerProvider
}

// newOptions starts from the environment and lets explicit options override it.
func newOptions(opts []Option) options {
	o := options{locationPrefix: os.Getenv("BUILDKITE_ANALYTICS_LOCATION_PREFIX")}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// SpanFilter decides whether a finished child span is exported to Buildkite.
// It never sees test.execution or phase spans.
type SpanFilter func(span sdktrace.ReadOnlySpan) bool

// WithTags adds run-level tags to every test execution as buildkite.tag.<key>.
func WithTags(tags map[string]string) Option {
	return func(o *options) {
		if o.tags == nil {
			o.tags = map[string]string{}
		}
		maps.Copy(o.tags, tags)
	}
}

// WithLocationPrefix prepends prefix to every test's file path, for suites
// that run from a subdirectory of the repository.
func WithLocationPrefix(prefix string) Option {
	return func(o *options) { o.locationPrefix = prefix }
}

// WithSpanFilter drops child spans the filter rejects before they are sent to
// Buildkite. Test spans are never filtered.
func WithSpanFilter(filter SpanFilter) Option {
	return func(o *options) { o.filter = filter }
}

// WithTracerProvider names the suite's own tracer provider. The collector
// attaches its child span forwarder to it and leaves its sampler, resource
// and processors alone. Without this option the collector attaches to the
// global provider when it is an SDK provider, installs its own when nobody
// has configured one, and otherwise exports test spans only.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

type spanProcessorRegistrar interface {
	RegisterSpanProcessor(sdktrace.SpanProcessor)
}

// collector is the configured state for one run in this process.
type collector struct {
	env            runEnv
	locationPrefix string
	tags           map[string]string
	runAttrs       []attribute.KeyValue
	links          []trace.Link

	testProvider *sdktrace.TracerProvider
	testTracer   trace.Tracer
	testStats    *streamStats

	// childProvider is the suite's own provider from WithTracerProvider;
	// nil means the global provider at the time each test starts.
	childProvider trace.TracerProvider

	childProcessor sdktrace.SpanProcessor
	childStats     *streamStats
	forwarder      *forwarder

	shutdownOnce  sync.Once
	abortDeadline func() time.Time
}

// current is the collector Main configured for this process.
var current atomic.Pointer[collector]

var errNoToken = errors.New("no token configured. Set BUILDKITE_TESTS_OTLP_TOKEN or BUILDKITE_ANALYTICS_TOKEN; test results will not be uploaded to Buildkite Test Engine")

// configure performs the mandatory test-span setup and, if that succeeds,
// the optional child-span setup.
func configure(ctx context.Context, opts options) (*collector, error) {
	token := apiToken()
	env := detectRunEnv()
	if token == "" {
		return nil, errNoToken
	}
	if !validRunKey(env.key) {
		// Wording shared with the other collectors.
		return nil, fmt.Errorf("run key %q is invalid. Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); it must be 1-255 printable ASCII characters without spaces", env.key)
	}
	endpointURL, err := validateEndpoint(endpoint())
	if err != nil {
		return nil, err
	}

	c := &collector{
		env:            env,
		locationPrefix: opts.locationPrefix,
		childProvider:  opts.tracerProvider,
		tags:           runTags(opts.tags),
		links:          jobSpanLinks(),
		testStats:      &streamStats{},
		childStats:     &streamStats{},
		abortDeadline:  sync.OnceValue(func() time.Time { return now().Add(processorTimeout) }),
	}
	c.runAttrs = runAttributes(env, opts.locationPrefix)
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

	if err := c.configureChildExport(ctx, opts, endpointURL, token, res); err != nil {
		warnf("OpenTelemetry child span export disabled: %v; test.execution export remains enabled", err)
	}
	return c, nil
}

// configureChildExport attaches the forwarder to the provider that owns the
// suite's instrumentation spans, per the ownership rules on WithTracerProvider.
// Child export is optional, so a provider that panics only costs child spans.
func (c *collector) configureChildExport(ctx context.Context, opts options, endpointURL *url.URL, token string, res *resource.Resource) error {
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
	fwd := newForwarder(countingProcessor{SpanProcessor: processor, stats: c.childStats}, opts.filter)

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

	tp := opts.tracerProvider
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
			warnf("Could not shut down the child span processor: %v", err)
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

// shutdown deactivates the forwarder so no new child spans enqueue, then
// shuts down the test stream and the child stream against one shared
// deadline.
func (c *collector) shutdown() {
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

// abort flushes both streams while a panic unwinds the test binary. It is
// a flush, not a shutdown, because go test runs the cleanups of the
// panicking test's ancestors afterwards and their results still need a
// working pipeline. Every abort shares the deadline the first one set.
func (c *collector) abort() {
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
func (c *collector) drain(ctx context.Context, what string, op func(context.Context) error) bool {
	err := op(ctx)
	if err == nil {
		return true
	}
	warnf("Could not %s: %v", what, err)
	return !errors.Is(err, context.DeadlineExceeded)
}

// reportDrops warns about test spans that were lost (TEST RESULTS MISSING)
// and about child spans that were lost (best effort). Drops are reported
// once per increase; spans still in flight at the deadline are reported as
// pending each time.
func (c *collector) reportDrops(testDrained, childDrained bool) {
	if test, unreported := c.testStats.unreported(testDrained); unreported {
		warnf("TEST RESULTS MISSING: OpenTelemetry dropped %d test.execution span(s) (%s).", test.dropped, test.reason)
		warnf("OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.")
	} else if test.pending > 0 {
		warnf("TEST RESULTS MISSING: %d test.execution span(s) were still exporting when the %s deadline passed and may not have reached Buildkite Test Engine.", test.pending, processorTimeout)
	}

	if child, unreported := c.childStats.unreported(childDrained); unreported {
		warnf("OpenTelemetry dropped %d child span(s) (%s); test.execution results are unaffected.", child.dropped, child.reason)
	} else if child.pending > 0 {
		warnf("OpenTelemetry child span export did not finish within %s; %d child span(s) may be missing. test.execution results are unaffected.", processorTimeout, child.pending)
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

// now is the realtime clock for span timestamps; tests replace it.
var now = time.Now
