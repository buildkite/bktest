// Suites that test their own tracing register a provider, a context manager
// and a propagator before each test and reset OpenTelemetry after it. The
// collector never blocks or replaces what they register, and their spans go
// to their provider alone.
const { trace, context, propagation } = require('@opentelemetry/api')
const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
const { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace-base')

const propagator = { inject() {}, extract: (context) => context, fields: () => ['own-header'] }
let exporter
let registered
beforeEach(() => {
  exporter = new InMemorySpanExporter()
  registered = [
    trace.setGlobalTracerProvider(new BasicTracerProvider({ spanProcessors: [new SimpleSpanProcessor(exporter)] })),
    context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable()),
    propagation.setGlobalPropagator(propagator),
  ]
})
afterEach(() => {
  trace.disable()
  context.disable()
  propagation.disable()
})

test.each(['own-a', 'own-b'])('own provider %s', (name) => {
  trace.getTracer('mine').startSpan(name).end()
  expect(registered).toEqual([true, true, true])
  expect(exporter.getFinishedSpans().map((span) => span.name)).toEqual([name])
})
