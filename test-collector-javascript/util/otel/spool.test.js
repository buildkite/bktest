const fs = require('fs')
const os = require('os')
const path = require('path')
const { createRequire } = require('module')
const { commitFile, parseFile, restoreSpans } = require('./spool')
const { loadSdk } = require('./sdk')

test('preserves non-finite numbers in span, event, link and distinct resource attributes on disk and on the wire', async () => {
  const sdk = loadSdk()
  const load = createRequire(require.resolve('@opentelemetry/exporter-trace-otlp-proto'))
  const { ProtobufTraceSerializer } = load('@opentelemetry/otlp-transformer')
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-spool-test-'))
  const providers = []
  const attributes = { nan: NaN, positive: Infinity, negative: -Infinity, array: [NaN, Infinity, -Infinity, 1] }
  try {
    const spans = [NaN, Infinity, -Infinity].map((number) => {
      const provider = new sdk.trace.BasicTracerProvider({ resource: sdk.resources.resourceFromAttributes({ ...attributes, distinct: number }) })
      providers.push(provider)
      const span = provider.getTracer('numbers').startSpan('child', {
        attributes,
        links: [{ context: { traceId: '0123456789abcdef0123456789abcdef', spanId: '0123456789abcdef', traceFlags: 1 }, attributes }],
      })
      span.addEvent('numbers', attributes)
      span.end()
      return span
    })
    commitFile(directory, 'numbers.test.js', { child: spans }, 'project-id')
    const files = fs.readdirSync(directory).map((name) => parseFile(fs.readFileSync(path.join(directory, name), 'utf8')))
    expect(files.find((file) => file.testPath)).toMatchObject({ projectId: 'project-id', testPath: 'numbers.test.js', streams: { test: [] } })
    const restored = restoreSpans(files.find((file) => file.streams.child).streams, sdk).child
    for (const [index, span] of restored.entries()) {
      expect(span.attributes).toEqual(attributes)
      expect(span.events[0].attributes).toEqual(attributes)
      expect(span.links[0].attributes).toEqual(attributes)
      expect(span.resource.attributes).toEqual({ ...attributes, distinct: [NaN, Infinity, -Infinity][index] })
    }
    expect(ProtobufTraceSerializer.serializeRequest(restored)).toEqual(ProtobufTraceSerializer.serializeRequest(spans))
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
    await Promise.all(providers.map((provider) => provider.shutdown()))
  }
})
