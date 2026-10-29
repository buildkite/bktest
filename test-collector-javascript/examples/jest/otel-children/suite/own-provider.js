// Suites that test their own tracing register a provider and a propagator
// before each test and reset OpenTelemetry after it. The collector never
// blocks or replaces what they register, and their spans go to their
// provider alone.
const { trace, propagation } = require('@opentelemetry/api')
const { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace-base')

const propagator = { inject() {}, extract: (context) => context, fields: () => ['own-header'] }
let exporter
let registered
beforeEach(() => {
  exporter = new InMemorySpanExporter()
  registered = [
    trace.setGlobalTracerProvider(new BasicTracerProvider({ spanProcessors: [new SimpleSpanProcessor(exporter)] })),
    propagation.setGlobalPropagator(propagator),
  ]
})
afterEach(() => {
  trace.disable()
  propagation.disable()
})

test.each(['own-a', 'own-b'])('own provider %s', (name) => {
  trace.getTracer('mine').startSpan(name).end()
  expect(registered).toEqual([true, true])
  expect(exporter.getFinishedSpans().map((span) => span.name)).toEqual([name])
})
