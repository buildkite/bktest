// A file that registers its own provider at load time, as suites testing
// their own tracing do. The collector must leave it alone: no forwarding and
// no instrumentation for this file, while files around it keep theirs.
const http = require('http')
const api = require('@opentelemetry/api')
const { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } = require('@opentelemetry/sdk-trace-base')

const exporter = new InMemorySpanExporter()
const provider = new BasicTracerProvider({ spanProcessors: [new SimpleSpanProcessor(exporter)] })
const registered = api.trace.setGlobalTracerProvider(provider)

test('own provider', async () => {
  if (!process.env.OTEL_CHILD_CASE?.startsWith('suite')) expect(registered).toBe(true)
  const server = http.createServer((req, res) => res.end('owned'))
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  await new Promise((resolve, reject) => {
    http.get(`http://127.0.0.1:${server.address().port}`, (res) => { res.resume(); res.on('end', resolve) }).on('error', reject)
  })
  await new Promise((resolve) => server.close(resolve))
  api.trace.getTracer('owner').startSpan('owner-child').end()
  if (registered) {
    // The collector installed no HTTP instrumentation for this file.
    expect(exporter.getFinishedSpans().map((span) => span.name)).toEqual(['owner-child'])
  }
})
