const api = require('@opentelemetry/api')
const { configure, createBuildkiteSpanProcessor, instrumentModule } = require('buildkite-test-collector/jest/otel')
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http')
const { UndiciInstrumentation } = require('@opentelemetry/instrumentation-undici')
const { GraphQLInstrumentation } = require('@opentelemetry/instrumentation-graphql')

const http = new HttpInstrumentation({ enabled: false, disableIncomingRequestInstrumentation: true })
const undici = new UndiciInstrumentation({ enabled: false })
const graphql = new GraphQLInstrumentation({ enabled: false })
const instrumentations = [http, undici, graphql]
const mode = process.env.OTEL_CHILD_CASE

configure({ instrumentations })
if (mode === 'bad-patch') instrumentModule(http, 'nonexistent-module', {}, '1.0.0')

if (mode?.startsWith('suite')) {
  const { BasicTracerProvider, AlwaysOffSampler, AlwaysOnSampler, InMemorySpanExporter, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace-base')
  const { resourceFromAttributes } = require('@opentelemetry/resources')
  const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
  const exported = new InMemorySpanExporter()
  const processors = [new SimpleSpanProcessor(exported)]
  if (mode !== 'suite-no-forwarder') processors.push(createBuildkiteSpanProcessor())
  const sampler = mode === 'suite-off' ? new AlwaysOffSampler() : new AlwaysOnSampler()
  const resource = resourceFromAttributes({ 'service.name': 'customer-service' })
  const provider = new BasicTracerProvider({ sampler, resource, spanProcessors: processors })
  const manager = new AsyncLocalStorageContextManager().enable()
  api.trace.setGlobalTracerProvider(provider)
  api.context.setGlobalContextManager(manager)
  global.customerProvider = provider
  for (const instrumentation of instrumentations) {
    instrumentation.setTracerProvider(provider)
    instrumentation.setConfig({ ...instrumentation.getConfig(), enabled: true })
    instrumentation.enable()
  }
  afterAll(async () => {
    expect(api.trace.getTracerProvider().getDelegate()).toBe(provider)
    await provider.forceFlush()
    const spans = exported.getFinishedSpans()
    if (mode === 'suite-off') expect(spans).toHaveLength(0)
    else {
      if (expect.getState().testPath.endsWith('/io.js')) expect(spans.some((s) => s.name === 'callback-child')).toBe(true)
      expect(spans.every((s) => s.resource === resource)).toBe(true)
      expect(spans.some((s) => s.name === 'test.execution')).toBe(false)
    }
    for (const instrumentation of instrumentations) instrumentation.disable()
    // The collector must not shut this provider or context manager down.
  })
}

// Core http shares its exports with Node, but patching explicitly also works
// when it was required before the instrumentation's require hook was enabled.
instrumentModule(http, 'http', require('http'), process.versions.node)
instrumentModule(http, 'https', require('https'), process.versions.node)
const version = require('graphql/package.json').version
instrumentModule(graphql, 'graphql/execution/execute.js', require('graphql/execution/execute.js'), version)
instrumentModule(graphql, 'graphql/language/parser.js', require('graphql/language/parser.js'), version)
instrumentModule(graphql, 'graphql/validation/validate.js', require('graphql/validation/validate.js'), version)

// A detached trace must never be forwarded.
beforeAll(() => api.trace.getTracer('fixture').startSpan('outside-test', { root: true }).end())
