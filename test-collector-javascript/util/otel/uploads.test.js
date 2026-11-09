const { execFile } = require('child_process')
const path = require('path')

const planReason = "Your organization's plan doesn't include OTel child spans, so this request wasn't stored. Test results are still accepted."
const limitReason = 'OTel trace ingest limit reached for this billing period'

// Like session.test.js, use plain Node for the stock exporter's dynamic HTTP
// imports, which Jest's VM cannot execute without experimental flags.
const scenario = async ({ status, testStatus = 200, reason, cap, late }) => {
  const fs = require('fs')
  const os = require('os')
  const path = require('path')
  const { startOtlpServer } = require('./e2e/support/otlpServer')
  const { loadSdk } = require('./util/otel/sdk')
  const { commitFile, MAX_RESIDENT_CHILDREN } = require('./util/otel/spool')
  const { createUploads } = require('./util/otel/uploads')
  const { createChildExport } = require('./util/otel/children')
  const warnings = []
  console.warn = (message) => warnings.push(message)
  process.env.BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE = '1'
  process.env.BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE = cap ? '2' : '2048'
  // google.rpc.Status { code: PERMISSION_DENIED, message }.
  const bytes = Buffer.from(reason || '')
  const length = bytes.length < 128 ? [bytes.length] : [(bytes.length & 127) | 128, bytes.length >> 7]
  const body = Buffer.concat([Buffer.from([8, 7, 18, ...length]), bytes])
  let requests = 0
  const server = await startOtlpServer({ respond: (_, headers) => {
    if (headers['buildkite-tests-span-stream'] !== 'child') return testStatus
    requests++
    return { status: status === 413 && requests > 1 ? 200 : status || 200, body }
  } })
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-uploads-'))
  const provider = new (loadSdk().trace.BasicTracerProvider)()
  const span = (name = 'child') => {
    const value = provider.getTracer('uploads').startSpan(name)
    value.end()
    return value
  }
  const settings = { endpoint: server.url, token: 'secret', runEnv: { key: 'uploads-test' }, spool: directory }
  const children = createChildExport(settings)
  try {
    if (cap || late) {
      for (let i = 0; i < 3; i++) {
        const forwarder = children.createForwarder()
        const accepted = []
        for (let j = 0; j < 5; j++) {
          const child = span()
          forwarder.addTrace(child.spanContext().traceId)
          if (forwarder.onStart(child)) accepted.push(child)
        }
        if (!late) accepted.forEach((child) => forwarder.onEnd(child))
        await forwarder.shutdown()
        if (late) {
          const write = fs.writeFileSync
          fs.writeFileSync = (file, ...args) => {
            if (/\.c\d+\.tmp$/.test(file)) throw new RangeError('Invalid string length')
            return write(file, ...args)
          }
          try { accepted.forEach((child) => forwarder.onEnd(child)) } finally { fs.writeFileSync = write }
        }
        commitFile(directory, `file-${i}`, { child: children.take(), test: [span('test.execution')] })
      }
    } else {
      const count = status === 403 ? MAX_RESIDENT_CHILDREN + 3 : 7
      commitFile(directory, 'tests.js', { child: Array(count).fill(span()), test: [span('test.execution')] })
    }
    const counts = await createUploads(settings, directory).drain()
    return { counts, requests, names: server.spans().map((span) => span.name), warnings }
  } finally {
    process.off('exit', children.reportDropped)
    await provider.shutdown()
    await server.close()
    fs.rmSync(directory, { recursive: true, force: true })
  }
}
const run = (options) => new Promise((resolve, reject) => {
  execFile(process.execPath, ['-e', `(${scenario})(${JSON.stringify(options)}).then(result => process.stdout.write(JSON.stringify(result))).catch(error => { console.error(error); process.exitCode = 1 })`],
    { cwd: path.join(__dirname, '../..') }, (error, stdout, stderr) => error ? reject(new Error(stderr)) : resolve(JSON.parse(stdout)))
})

test.each([planReason, limitReason])('stops child uploads on 403 and counts resident and unread children: %s', async (reason) => {
  const result = await run({ status: 403, reason })
  expect(result.counts).toEqual({ test: 0, child: 16387 })
  expect(result.requests).toBeGreaterThan(0)
  expect(result.requests).toBeLessThanOrEqual(4) // Only requests already in flight.
  expect(result.names).toEqual(['test.execution'])
  expect(result.warnings).toEqual([expect.stringContaining(`dropped 16387 child span(s) in total (HTTP 403: ${reason}; child uploads stopped for this run)`)])
}, 15000)

test('keeps other child 4xx per batch and sends later children', async () => {
  const result = await run({ status: 413 })
  expect(result.counts).toEqual({ test: 0, child: 1 })
  expect(result.requests).toBe(7)
  expect(result.names.filter((name) => name === 'child')).toHaveLength(6)
  expect(result.warnings).toEqual([expect.stringContaining('dropped 1 child span(s) in total (HTTP 413)')])
})

test('reports each lane\'s own failure and keeps sending children after a test-stream 403', async () => {
  const result = await run({ status: 413, testStatus: 403 })
  expect(result.counts).toEqual({ test: 1, child: 1 })
  expect(result.requests).toBe(7)
  expect(result.names).toEqual(Array(6).fill('child'))
  expect(result.warnings).toEqual([
    expect.stringContaining('TEST RESULTS MISSING: 1 test.execution span(s), 1 child span(s) were not acknowledged (HTTP 403)'),
    expect.stringContaining('dropped 1 child span(s) in total (HTTP 413)'),
  ])
})

test('folds per-file cap drops into one run total across files', async () => {
  const result = await run({ cap: true })
  expect(result.counts).toEqual({ test: 0, child: 9 })
  expect(result.names.filter((name) => name === 'test.execution')).toHaveLength(3)
  expect(result.names.filter((name) => name === 'child')).toHaveLength(6)
  expect(result.warnings).toEqual([expect.stringContaining('dropped 9 child span(s) in total (per-file cap 2)')])
})

test('warns once for repeated late child commit failures and includes them in the total', async () => {
  const result = await run({ late: true })
  expect(result.counts).toEqual({ test: 0, child: 15 })
  expect(result.names).toEqual(['test.execution', 'test.execution', 'test.execution'])
  expect(result.warnings).toEqual([
    expect.stringContaining('OpenTelemetry dropped child spans (could not commit child spool: RangeError: Invalid string length)'),
    expect.stringContaining('OpenTelemetry dropped 15 child span(s) in total'),
  ])
})

test('reports unfinished child counts from every worker, once per worker', () => {
  const fs = require('fs')
  const os = require('os')
  const { createChildExport } = require('./children')
  const spool = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-unfinished-'))
  const warnings = jest.spyOn(console, 'warn').mockImplementation()
  const workers = [createChildExport({ spool }), createChildExport({ spool })]
  try {
    for (const worker of workers) {
      const forwarder = worker.createForwarder()
      forwarder.addTrace('trace')
      forwarder.onStart({ spanContext: () => ({ traceId: 'trace' }) })
      worker.reportDropped()
      worker.reportDropped()
    }
    expect(warnings.mock.calls).toEqual([
      [expect.stringContaining('dropped 1 child span(s) (still open when the worker exited: 1)')],
      [expect.stringContaining('dropped 1 child span(s) (still open when the worker exited: 1)')],
    ])
  } finally {
    workers.forEach((worker) => process.off('exit', worker.reportDropped))
    warnings.mockRestore()
    fs.rmSync(spool, { recursive: true, force: true })
  }
})
