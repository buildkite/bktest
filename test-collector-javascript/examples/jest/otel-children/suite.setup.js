// A suite that owns its provider, context manager and instrumentation in the
// test sandbox. The collector must not replace or reconfigure any of it.
const fs = require('fs')
const api = require('@opentelemetry/api')
const { createBuildkiteSpanProcessor } = require('buildkite-test-collector/jest/otel')
const { BasicTracerProvider, AlwaysOffSampler, AlwaysOnSampler, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace-base')
const { resourceFromAttributes } = require('@opentelemetry/resources')
const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http')
const { UndiciInstrumentation } = require('@opentelemetry/instrumentation-undici')

const mode = process.env.OTEL_CHILD_CASE

// Writes what the suite's own exporter receives, including spans that end
// after the file's last hook, for the e2e test to inspect.
const fileExporter = {
  export(spans, done) {
    const lines = spans.map((span) => JSON.stringify({ name: span.name, kind: span.kind, attributes: span.attributes, resource: span.resource.attributes }))
    fs.appendFileSync(process.env.OTEL_SUITE_SPANS_FILE, lines.map((line) => `${line}\n`).join(''))
    done({ code: 0 })
  },
  shutdown: async () => {},
}

const processors = [new SimpleSpanProcessor(fileExporter)]
if (mode !== 'suite-no-forwarder') processors.push(createBuildkiteSpanProcessor())
const provider = new BasicTracerProvider({
  sampler: mode === 'suite-off' ? new AlwaysOffSampler() : new AlwaysOnSampler(),
  resource: resourceFromAttributes({ 'service.name': 'customer-service' }),
  spanProcessors: processors,
})
api.trace.setGlobalTracerProvider(provider)
api.context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable())

// Inside Jest, the suite has to patch Node's shared http module itself.
const http = new HttpInstrumentation()
const undici = new UndiciInstrumentation()
for (const definition of http.getModuleDefinitions()) definition.patch(require(definition.name))

afterAll(() => {
  expect(api.trace.getTracerProvider().getDelegate()).toBe(provider)
  undici.disable()
  // The suite's http patch stays in place until the process exits, so the
  // collector's final export for this file passes through it.
})
