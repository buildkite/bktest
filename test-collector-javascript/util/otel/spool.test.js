const fs = require('fs')
const os = require('os')
const path = require('path')
const { createRequire } = require('module')
const { commitFile, parseFile, restoreSpans } = require('./spool')
const { loadSdk } = require('./sdk')

test.each(['serialize', 'write', 'rename'])('keeps test commits when child %s fails, warning only once per run', async (failure) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-spool-failure-'))
  const sdk = loadSdk()
  const provider = new sdk.trace.TracerProvider()
  const span = provider.getTracer('spool').startSpan('test.execution')
  span.end()
  const warn = jest.spyOn(console, 'warn').mockImplementation()
  const method = failure === 'rename' ? 'renameSync' : 'writeFileSync'
  const original = fs[method]
  const io = jest.spyOn(fs, method).mockImplementation((file, ...args) => {
    if (failure !== 'serialize' && /\.c\d+\.tmp$/.test(file)) throw new Error('child I/O failed')
    return original(file, ...args)
  })
  const child = failure === 'serialize' ? {
    ...span, spanContext: () => span.spanContext(),
    attributes: { uncloneable() {} },
  } : span
  try {
    for (let i = 0; i < 3; i++) commitFile(directory, `test-${i}.js`, { test: [span], child: [child] })
    const commits = fs.readdirSync(directory).filter((name) => name.endsWith('.spool'))
      .map((name) => parseFile(fs.readFileSync(path.join(directory, name))))
    expect(commits.map((file) => file.testPath).sort()).toEqual(['test-0.js', 'test-1.js', 'test-2.js'])
    expect(commits.every((file) => file.streams.test[0].name === 'test.execution')).toBe(true)
    expect(warn.mock.calls).toEqual([[expect.stringMatching(/OpenTelemetry dropped.*child.*test.execution results are unaffected/)]])
    expect(fs.readdirSync(directory).some((name) => name.endsWith('.tmp'))).toBe(false)
  } finally {
    io.mockRestore()
    warn.mockRestore()
    fs.rmSync(directory, { recursive: true, force: true })
    await provider.shutdown()
  }
})

test('preserves non-finite numbers in span, event, link and distinct resource attributes on disk and on the wire', async () => {
  const sdk = loadSdk()
  const load = createRequire(require.resolve('@opentelemetry/exporter-trace-otlp-proto'))
  const { ProtobufTraceSerializer } = load('@opentelemetry/otlp-transformer')
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-spool-test-'))
  const providers = []
  const attributes = { $number: 'Infinity', other: 'value', attributes: Infinity,
    nan: NaN, positive: Infinity, negative: -Infinity, array: [NaN, Infinity, -Infinity, 1] }
  // SDK 3 supports structured span/link values, but events and resources still
  // accept only simple attributes. Do not mistake SDK rejection for codec loss.
  const eventAttributes = { ...attributes }
  if (require('semver').gte(sdk.apiVersion, '1.10.0')) Object.assign(attributes, {
    bytes: new Uint8Array([1, 127, 255]), nested: { map: { value: null } }, mixed: [1, 'two', false, null, { three: 3 }],
  })
  try {
    const spans = [NaN, Infinity, -Infinity].map((number) => {
      const provider = new sdk.trace.TracerProvider({ resource: sdk.resources.resourceFromAttributes({ ...eventAttributes, distinct: number }) })
      providers.push(provider)
      const span = provider.getTracer('numbers').startSpan('child', {
        attributes,
        links: [{ context: { traceId: '0123456789abcdef0123456789abcdef', spanId: '0123456789abcdef', traceFlags: 1 }, attributes }],
      })
      span.addEvent('numbers', eventAttributes)
      span.addEvent('marker-only', { $number: 'NaN' })
      span.end()
      return span
    })
    commitFile(directory, 'numbers.test.js', { child: spans }, 'project-id')
    const files = fs.readdirSync(directory).map((name) => parseFile(fs.readFileSync(path.join(directory, name))))
    expect(files.find((file) => file.testPath)).toMatchObject({ projectId: 'project-id', testPath: 'numbers.test.js', streams: { test: [] } })
    const restored = restoreSpans(files.find((file) => file.streams.child).streams, sdk).child
    for (const [index, span] of restored.entries()) {
      expect(span.attributes).toEqual(attributes)
      expect(span.events[0].attributes).toEqual(eventAttributes)
      expect(span.events[1].attributes).toEqual({ $number: 'NaN' })
      expect(span.links[0].attributes).toEqual(attributes)
      expect(span.resource.attributes).toEqual({ ...eventAttributes, distinct: [NaN, Infinity, -Infinity][index] })
    }
    expect(ProtobufTraceSerializer.serializeRequest(restored)).toEqual(ProtobufTraceSerializer.serializeRequest(spans))
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
    await Promise.all(providers.map((provider) => provider.shutdown()))
  }
})
