const { execFile } = require('child_process')
const path = require('path')
const { version } = require('../../package.json')
const { createSession, sessionFor } = require('./session')

const consoleWarnSpy = jest.spyOn(console, 'warn').mockImplementation()
const warnings = () => consoleWarnSpy.mock.calls.map(([message]) => message)

const record = (overrides = {}) => ({
  id: 'id-1',
  scope: 'sum',
  name: 'adds',
  location: 'sum.test.js:3',
  file_name: 'sum.test.js',
  result: 'passed',
  failure_reason: undefined,
  failure_expanded: [{ expanded: [] }],
  ...overrides,
})

const failing = record({
  result: 'failed',
  failure_reason: 'expect(received).toBe(expected)',
  failure_expanded: [{ expanded: ['Expected: 2', 'Received: 3'] }],
})

const settings = (overrides = {}) => ({
  endpoint: 'http://127.0.0.1:9/v1/traces',
  token: 'secret',
  runEnv: { ci: 'buildkite', key: 'run-1', collector: 'js-buildkite-test-collector', version },
  tags: {},
  testRunner: 'jest',
  frameworkVersion: '29.7.0',
  ...overrides,
})

// Exports go through a real OTLP exporter in a child process (see runSession.js).
const runSession = (scenario) => new Promise((resolve, reject) => {
  const driver = path.join(__dirname, '../../e2e/support/runSession.js')
  const env = { ...process.env, BUILDKITE_AGENT_ID: 'agent-1' }
  for (const key of Object.keys(env)) {
    if (/^(OTEL_|TRACEPARENT|TRACESTATE|BUILDKITE_TESTS_OTEL_)/.test(key)) delete env[key]
  }
  execFile(process.execPath, [driver, JSON.stringify({ tests: [{ start: record(), line: 3 }], ...scenario })], { env }, (error, stdout, stderr) => {
    if (error) return reject(new Error(`${error.message}\n${stderr}`))
    resolve(JSON.parse(stdout))
  })
})

const OLD_ENV = process.env

beforeEach(() => {
  process.env = { ...OLD_ENV }
  consoleWarnSpy.mockClear()
})

afterAll(() => {
  process.env = OLD_ENV
})

describe('createSession', () => {
  it('exports one root test.execution span per test with the Buildkite headers', async () => {
    const { requests, resource, scope, spans, warnings } = await runSession({ tests: [{ start: record(), line: 3, startTime: 1000, endTime: 1500 }] })

    expect(requests).toHaveLength(1)
    expect(requests[0].headers).toMatchObject({
      authorization: 'Token token="secret"',
      'buildkite-tests-run-key': 'run-1',
      'buildkite-tests-span-stream': 'test',
      'content-type': 'application/x-protobuf',
      'content-encoding': 'gzip',
    })
    expect(scope).toEqual({ name: 'buildkite-test-collector', version })
    expect(resource).toMatchObject({ 'cicd.worker.id': 'agent-1', 'vcs.ref.head.name': 'main', 'telemetry.sdk.language': 'nodejs' })

    expect(spans).toHaveLength(1)
    const [span] = spans
    expect(span.name).toBe('test.execution')
    expect(span.parentSpanId).toBe('')
    expect(span.kind).toBe(1)
    expect(span.traceId).toMatch(/^[0-9a-f]{32}$/)
    expect(span.startTimeUnixNano).toBe('1000000000')
    expect(span.endTimeUnixNano).toBe('1500000000')
    expect(span.status.code).toBe(0)
    expect(span.events).toEqual([])
    expect(span.attributes).toEqual({
      'buildkite.execution.via': 'otlp',
      'buildkite.run_key': 'run-1',
      'test.case.result.status': 'pass',
      'buildkite.test.scope': 'sum',
      'buildkite.test.name': 'adds',
      'test.case.name': 'sum adds',
      'test.suite.name': 'sum',
      'code.file.path': 'sum.test.js',
      'code.line.number': '3',
      'buildkite.test.execution.external_id': 'id-1',
      'buildkite.collector.name': 'js-buildkite-test-collector',
      'buildkite.collector.version': version,
      'buildkite.test.framework.name': 'jest',
      'buildkite.test.framework.version': '29.7.0',
      'buildkite.tag.ci.worker.id': 'agent-1',
      'buildkite.tag.hello': 'world',
    })
    expect(span.attributeTypes['code.line.number']).toBe('int')
    expect(warnings).toEqual([])
  })

  it('exports optional columns as integers and lets stringified per-test tags override run tags', async () => {
    const { spans } = await runSession({ tests: [
      { start: record(), line: 3, column: 7, finish: record({ tags: { hello: 'test', 'ci.worker.id': 'test-worker', count: 42, enabled: false } }) },
      { start: record(), line: 3 },
    ] })

    expect(spans).toHaveLength(2)
    expect(spans[0].attributes).toMatchObject({
      'code.line.number': '3',
      'code.column.number': '7',
      'buildkite.tag.hello': 'test',
      'buildkite.tag.ci.worker.id': 'test-worker',
      'buildkite.tag.count': '42',
      'buildkite.tag.enabled': 'false',
    })
    expect(spans[0].attributeTypes['code.column.number']).toBe('int')
    expect(spans[0].attributeTypes['buildkite.tag.count']).toBe('string')
    expect(spans[1].attributes).not.toHaveProperty('code.column.number')
    expect(spans[1].attributes['buildkite.tag.hello']).toBe('world')
    expect(spans[1].attributes['buildkite.tag.ci.worker.id']).toBe('agent-1')
    expect(spans[1].attributes).not.toHaveProperty('buildkite.tag.count')
  })

  it('records a failure as an ERROR status with the reason and exception events', async () => {
    const { spans } = await runSession({ tests: [{ start: record(), finish: failing, line: 3 }] })

    const [span] = spans
    expect(span.attributes['test.case.result.status']).toBe('fail')
    expect(span.status).toEqual({ code: 2, message: 'expect(received).toBe(expected)' })
    expect(span.events).toEqual([
      expect.objectContaining({ name: 'exception', attributes: { 'exception.message': 'Expected: 2\nReceived: 3' } }),
    ])
  })

  it('maps skipped results and leaves the placeholder when the result is unknown', async () => {
    const { spans } = await runSession({
      tests: [
        { start: record(), finish: record({ result: 'pending' }) },
        { start: record(), finish: record({ result: 'nonsense' }) },
      ],
    })

    expect(spans.map((span) => span.attributes['test.case.result.status'])).toEqual(['skipped', 'unset'])
  })

  it('exports long backtraces with a truncation marker and no split surrogate pairs', async () => {
    const { spans } = await runSession({ tests: [{
      start: record(),
      finish: record({
        result: 'failed',
        failure_expanded: [
          { backtrace: ['s'.repeat(15000)] },
          { backtrace: [`${'s'.repeat(10239)}😀${'s'.repeat(100)}`] },
        ],
      }),
    }] })

    const stacks = spans[0].events.map((event) => event.attributes['exception.stacktrace'])
    expect(stacks[0]).toHaveLength(10243)
    expect(stacks).toEqual([`${'s'.repeat(10240)}...`, `${'s'.repeat(10239)}...`])
  })

  it('keeps the identity attributes when OTEL_* limits, sampler and exporter env are hostile', async () => {
    const { requests, spans } = await runSession({
      env: {
        OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT: '2',
        OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT: '3',
        OTEL_SPAN_EVENT_COUNT_LIMIT: '0',
        OTEL_TRACES_SAMPLER: 'always_off',
        OTEL_EXPORTER_OTLP_HEADERS: 'authorization=garbage,x-env-only=1',
        OTEL_EXPORTER_OTLP_TRACES_HEADERS: 'authorization=garbage2,x-env-traces=1',
        OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: 'http/json',
        OTEL_EXPORTER_OTLP_ENDPOINT: 'http://127.0.0.1:9/v1/traces',
        OTEL_EXPORTER_OTLP_COMPRESSION: 'none',
        OTEL_BSP_MAX_QUEUE_SIZE: '1',
      },
      tests: [{ start: record(), finish: failing, line: 3 }, { start: record(), line: 3 }],
    })

    expect(requests).toHaveLength(1)
    expect(requests[0].headers.authorization).toBe('Token token="secret"')
    expect(requests[0].headers['content-type']).toBe('application/x-protobuf')
    expect(requests[0].headers['content-encoding']).toBe('gzip')
    expect(requests[0].headers).not.toHaveProperty('x-env-only')
    expect(requests[0].headers).not.toHaveProperty('x-env-traces')
    expect(requests[0].decodeError).toBeNull()

    expect(spans).toHaveLength(2)
    expect(spans[0].attributes['buildkite.test.name']).toBe('adds')
    expect(spans[0].attributes['buildkite.tag.hello']).toBe('world')
    expect(spans[0].droppedAttributesCount).toBe(0)
    expect(spans[0].events).toHaveLength(1)
  })

  it('lets the collector resource attributes win over OTEL_* detection without hiding what it lacks', async () => {
    const env = { OTEL_SERVICE_NAME: 'from-env', OTEL_RESOURCE_ATTRIBUTES: 'service.namespace=env-org,vcs.ref.head.name=env-branch' }
    const detected = await runSession({ env })
    expect(detected.resource).toMatchObject({ 'service.name': 'from-env', 'service.namespace': 'env-org', 'vcs.ref.head.name': 'main' })

    const own = await runSession({ env: { ...env, BUILDKITE_TEST_ENGINE_SUITE_SLUG: 'my-suite' } })
    expect(own.resource['service.name']).toBe('my-suite')
  })

  it('links the span to the job span from TRACEPARENT and ignores malformed values', async () => {
    const linked = await runSession({ env: { TRACEPARENT: '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01', TRACESTATE: 'vendor=abc' } })
    expect(linked.spans[0].links).toEqual([{ traceId: '0af7651916cd43dd8448eb211c80319c', spanId: 'b7ad6b7169203331', traceState: 'vendor=abc' }])
    expect(linked.spans[0].parentSpanId).toBe('')
    expect(linked.spans[0].traceId).not.toBe('0af7651916cd43dd8448eb211c80319c')

    for (const value of ['not-a-traceparent', '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-extra', 'ff-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01']) {
      const malformed = await runSession({ env: { TRACEPARENT: value } })
      expect(malformed.spans[0].links).toEqual([])
      expect(malformed.warnings).toEqual([])
    }
  })

  it('generates distinct, valid trace and span ids even when Math.random is stubbed', async () => {
    const { spans } = await runSession({
      stubMathRandom: true,
      tests: [{ start: record(), line: 3 }, { start: record({ id: 'id-2', name: 'subtracts' }), line: 9 }],
    })

    expect(spans).toHaveLength(2)
    expect(new Set(spans.map((span) => span.spanId)).size).toBe(2)
    expect(new Set(spans.map((span) => span.traceId)).size).toBe(2)
    for (const span of spans) {
      expect(span.traceId).toMatch(/^[0-9a-f]{32}$/)
      expect(span.traceId).not.toBe('0'.repeat(32))
      expect(span.spanId).toMatch(/^[0-9a-f]{16}$/)
      expect(span.spanId).not.toBe('0'.repeat(16))
    }
  })

  it('falls back to the SDK clock when the end time is before the start time', async () => {
    const before = BigInt(Date.now()) * 1000000n
    const { spans } = await runSession({ tests: [{ start: record(), line: 3, startTime: Date.now(), endTime: 5 }] })

    const [span] = spans
    expect(BigInt(span.endTimeUnixNano) >= BigInt(span.startTimeUnixNano)).toBe(true)
    expect(BigInt(span.endTimeUnixNano) >= before).toBe(true)
  })

  it('still finishes the span when the result cannot be described', async () => {
    const { spans, warnings } = await runSession({ tests: [{ start: record(), finish: null, line: 3 }] })

    expect(spans).toHaveLength(1)
    expect(spans[0].attributes['test.case.result.status']).toBe('unset')
    expect(warnings).toEqual([expect.stringMatching(/Could not record the OpenTelemetry test result: TypeError/)])
  })

  it('retries a 503 with the same stream header and forgets the failure once it succeeds', async () => {
    const { requests, spans, flushed } = await runSession({ respond: '503-once', forceFlush: true })

    expect(flushed.requests).toEqual([503, 200])
    expect(requests.map((request) => request.headers['buildkite-tests-span-stream'])).toEqual(['test', 'test'])
    expect(spans).toHaveLength(1)
    expect(flushed.metrics).toMatchObject({ exported: 1, failed: 0, lastFailure: null })
    expect(flushed.warnings).toEqual([])
  })

  it('waits for a batch the processor timer already handed to the exporter, retries included', async () => {
    const { flushed } = await runSession({ respond: '503-once', waitBeforeFlush: 1200, forceFlush: true })

    expect(flushed.requests).toEqual([503, 200])
    expect(flushed.metrics).toMatchObject({ exported: 1, failed: 0 })
  })

  it('warns TEST RESULTS MISSING once, naming the status, when export fails for good', async () => {
    const { requests, flushed, warnings } = await runSession({ respond: 401, tests: [{ start: record() }, { start: record() }], forceFlush: true })

    expect(requests).toHaveLength(1)
    expect(flushed.warnings).toEqual([
      expect.stringMatching(/^\[buildkite-test-collector\] TEST RESULTS MISSING: OpenTelemetry dropped 2 test\.execution span\(s\) \(export-failure, last OTLP failure: 401\)\./),
    ])
    expect(warnings).toEqual(flushed.warnings)
  })

  it('names the network error when the endpoint is unreachable', async () => {
    const { requests, flushed, warnings } = await runSession({ unreachable: true, forceFlush: true })

    expect(requests).toHaveLength(0)
    expect(flushed.warnings).toEqual([expect.stringMatching(/dropped 1 test\.execution span\(s\) \(export-failure, last OTLP failure: ECONNREFUSED\)/)])
    expect(warnings).toEqual(flushed.warnings)
  }, 15000)

  it('counts spans the full queue rejected as not exported', async () => {
    const { spans, flushed, warnings } = await runSession({
      env: { BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE: '2', BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE: '2' },
      tests: Array.from({ length: 5 }, () => ({ start: record(), line: 3 })),
      forceFlush: true,
    })

    expect(spans).toHaveLength(4)
    expect(flushed.warnings).toEqual([expect.stringMatching(/dropped 1 test\.execution span\(s\) \(not-exported\)/)])
    expect(warnings).toEqual(flushed.warnings)
  })

  it('validates the queue and batch size overrides', async () => {
    process.env.BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE = 'lots'
    await createSession(settings()).shutdown()
    expect(warnings()).toEqual([expect.stringMatching(/BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE must be a positive integer up to 2147483647, using default 8192/)])

    consoleWarnSpy.mockClear()
    process.env.BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE = '10'
    process.env.BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE = '11'
    await createSession(settings()).shutdown()
    expect(warnings()).toEqual([expect.stringMatching(/BATCH_SIZE must be <= BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE; using defaults \(batch 120, queue 8192\)/)])
  })
})

describe('sessionFor', () => {
  it('reuses the process session and warns when a different run key is requested', async () => {
    const first = sessionFor(settings())
    expect(sessionFor(settings())).toBe(first)
    expect(warnings()).toEqual([])

    expect(sessionFor(settings({ runEnv: { ...settings().runEnv, key: 'run-2' } }))).toBe(first)
    expect(warnings()).toEqual([expect.stringMatching(/already configured for run "run-1" and cannot switch to run "run-2"/)])
    await first.shutdown()
  })

  it('starts a new run once the shared session has been shut down', async () => {
    const first = sessionFor(settings())
    await first.shutdown()
    const second = sessionFor(settings({ runEnv: { ...settings().runEnv, key: 'run-3' } }))
    expect(second).not.toBe(first)
    expect(second.runKey).toBe('run-3')
    await second.shutdown()
  })
})
