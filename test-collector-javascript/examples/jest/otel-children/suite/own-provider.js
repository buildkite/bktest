// Suites that test their own tracing register a provider, a context manager
// and a propagator before each test and reset OpenTelemetry after it. The
// collector never blocks or replaces what they register, and their spans go
// to their provider alone.
const { trace, context, propagation } = require('@opentelemetry/api')
const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
const { TracerProvider, InMemorySpanExporter, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace')

const propagator = { inject() {}, extract: (context) => context, fields: () => ['own-header'] }
let appTracer = trace.getTracer('cached-app')
let exporter
let registered
beforeEach(() => {
  // Prime the API ProxyTracer's delegate while the collector owns the test.
  appTracer.startSpan('before-takeover').end()
  exporter = new InMemorySpanExporter()
  registered = [
    trace.setGlobalTracerProvider(new TracerProvider({ spanProcessors: [new SimpleSpanProcessor({ exporter })] })),
    context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable()),
    propagation.setGlobalPropagator(propagator),
  ]
})
afterEach(() => {
  trace.disable()
  context.disable()
  propagation.disable()
  appTracer = trace.getTracer('cached-app')
})

test.each(['own-a', 'own-b'])('own provider %s', (name) => {
  appTracer.startActiveSpan('after-takeover', (span) => {
    context.with(trace.setSpan(context.active(), span), () => {
      trace.getTracer('mine').startSpan(name).end()
    })
    span.end()
  })
  expect(registered).toEqual([true, true, true])
  const spans = exporter.getFinishedSpans()
  expect(spans.map((span) => span.name)).toEqual([name, 'after-takeover'])
  expect(spans[0].parentSpanContext).toEqual(spans[1].spanContext())
})
