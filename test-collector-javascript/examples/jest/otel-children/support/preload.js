// OpenTelemetry configured for the whole Jest process with --require, outside
// any test sandbox: a global provider, context manager and HTTP
// instrumentation, recording every span to a file.
const fs = require('fs')
const api = require('@opentelemetry/api')
const { BasicTracerProvider, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace-base')
const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http')

const provider = new BasicTracerProvider({
  spanProcessors: [new SimpleSpanProcessor({
    export(spans, done) {
      fs.appendFileSync(process.env.OTEL_SUITE_SPANS_FILE, spans.map((span) => `${JSON.stringify({ name: span.name, kind: span.kind, attributes: span.attributes })}\n`).join(''))
      done({ code: 0 })
    },
    shutdown: async () => {},
  })],
})
api.trace.setGlobalTracerProvider(provider)
api.context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable())
const http = new HttpInstrumentation()
http.setTracerProvider(provider)
http.enable()
