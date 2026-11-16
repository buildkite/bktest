// The app under test's own tracing, as a customer would already have it.
// Apps usually start @opentelemetry/sdk-node, which reads OTEL_TRACES_SAMPLER
// itself; this one is built from the collector's dev dependencies.
const api = require('@opentelemetry/api')
const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
const { W3CTraceContextPropagator } = require('@opentelemetry/core')
const { OTLPTraceExporter } = require('@opentelemetry/exporter-trace-otlp-proto')
const { registerInstrumentations } = require('@opentelemetry/instrumentation')
const { ExpressInstrumentation } = require('@opentelemetry/instrumentation-express')
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http')
const { resourceFromAttributes } = require('@opentelemetry/resources')
const { AlwaysOffSampler, AlwaysOnSampler, ParentBasedSampler, SimpleSpanProcessor, TracerProvider } = require('@opentelemetry/sdk-trace')

const root = process.env.OTEL_TRACES_SAMPLER === 'parentbased_always_off' ? new AlwaysOffSampler() : new AlwaysOnSampler()
const provider = new TracerProvider({
  resource: resourceFromAttributes({ 'service.name': 'playwright-e2e-app' }),
  sampler: new ParentBasedSampler({ root }),
  // Export per span: Playwright before 1.50 SIGKILLs the webServer at the end.
  spanProcessors: [new SimpleSpanProcessor({ exporter: new OTLPTraceExporter() })],
})
api.trace.setGlobalTracerProvider(provider)
api.context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable())
api.propagation.setGlobalPropagator(new W3CTraceContextPropagator())
registerInstrumentations({ instrumentations: [new HttpInstrumentation(), new ExpressInstrumentation()] })

process.once('SIGTERM', () => provider.shutdown().finally(() => process.exit(0)))
