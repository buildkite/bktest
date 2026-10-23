const { exec, execFile } = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')
const { startOtlpServer } = require('./support/otlpServer')

// examples/jest/otel.config.js runs: example.test.js (2), passed.test.js (1),
// otel/outcomes.js (9) and otel/retries.js (2 tests, 2 attempts each).
const EXPECTED_SPANS = 16
const EXPECTED_JSON_RESULTS = 14
const jestVersion = require(require.resolve('jest/package.json', { paths: [path.join(__dirname, '../examples/jest')] })).version
const jestMajor = Number(jestVersion.split('.')[0])

const run = (command, env) => new Promise((resolve) => {
  const cwd = path.join(__dirname, '../examples/jest')
  const jestBin = require.resolve('jest/bin/jest', { paths: [cwd] })
  const selectedJest = command.replace(/^jest\b/, `"${process.execPath}" "${jestBin}"`)
  exec(selectedJest, { cwd, env: { ...process.env, ...env } }, (error, stdout, stderr) => {
    resolve({ code: error ? error.code : 0, stdout, stderr })
  })
})

const jsonUpload = (stdout) => {
  const match = stdout.match(/Test Engine Sending: ({.*})/m)
  return match ? JSON.parse(match[1]).data.data : null
}

const identity = ({ scope, name, location, file_name }) => JSON.stringify([scope, name, location, file_name])

const spanRecord = (span) => {
  const a = span.attributes
  const line = a['code.line.number']
  return {
    scope: a['buildkite.test.scope'],
    name: a['buildkite.test.name'],
    location: line === undefined ? null : `${a['code.file.path']}:${line}`,
    file_name: a['code.file.path'],
    result: a['test.case.result.status'],
    failure_reason: span.status.message,
  }
}

const sortedBy = (records, key) => [...records].sort((a, b) => key(a).localeCompare(key(b)))

describe('examples/jest in OTel mode', () => {
  let server
  const baseEnv = {
    BUILDKITE_ANALYTICS_TOKEN: undefined,
    BUILDKITE_ANALYTICS_DEBUG_ENABLED: 'true',
    BUILDKITE_ANALYTICS_KEY: 'otel-e2e-run',
    BUILDKITE_AGENT_ID: 'agent-e2e',
    TRACEPARENT: '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01',
  }

  afterEach(async () => {
    if (server) await server.close()
    server = null
  })

  // The CI matrix sets the version it installed into examples/jest.
  test('runs the selected Jest version', () => {
    expect(jestVersion).toBe(process.env.EXPECTED_JEST_VERSION || jestVersion)
  })

  // examples/jest/otel-children.config.js: 21 tests in io.js, again.js,
  // cache.js, empty.js and owner.js; again.js retries one test, which fails
  // once.
  const CHILD_FIXTURE_ROOTS = 22
  const CLIENT = 3
  const SERVER = 2

  // Runs the child span fixtures against a fresh receiver and indexes what
  // arrived. `ownRequests` are spans of the collector's own OTLP requests,
  // which must never be recorded, whoever's instrumentation sees them.
  const runChildren = async (mode, { args = '--runInBand', env = {}, respond } = {}) => {
    server = await startOtlpServer({ respond })
    const port = new URL(server.url).port
    const result = await run(`jest --config otel-children.config.js ${args}`, {
      ...baseEnv,
      OTEL_CHILD_CASE: mode,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      ...env,
    })
    const spans = server.spans()
    const roots = spans.filter((s) => s.name === 'test.execution')
    const children = spans.filter((s) => s.name !== 'test.execution')
    const byId = new Map(spans.map((s) => [s.spanId, s]))
    const rootsNamed = (name) => roots.filter((s) => s.attributes['buildkite.test.name'] === name)
    const isOwnRequest = (span) => span.name.startsWith('POST') ||
      Object.values(span.attributes).some((value) => String(value) === port || String(value).includes(`:${port}/`))
    // The test span a child descends from, following parent links.
    const rootOf = (span) => {
      const visited = new Set()
      let current = span
      while (current && current.parentSpanId) {
        if (visited.has(current.spanId)) return null
        visited.add(current.spanId)
        current = byId.get(current.parentSpanId)
      }
      return current && current.name === 'test.execution' && current.traceId === span.traceId ? current : null
    }
    const childrenOf = (root, predicate = () => true) => children.filter((s) => rootOf(s) === root && predicate(s))
    const warnings = result.stderr.split('\n').filter((line) => line.includes('[buildkite-test-collector]'))
    return { result, port, roots, children, rootsNamed, childrenOf, rootOf, isOwnRequest, warnings }
  }

  const named = (name) => (span) => span.name === name
  const clientGet = (span) => span.name === 'GET' && span.kind === CLIENT

  const expectAllResults = ({ result, roots }) => {
    expect(result.code).toBe(1) // intentional failures in empty.js
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    expect(result.stderr).not.toMatch(/TEST RESULTS MISSING/)
    expect(roots).toHaveLength(CHILD_FIXTURE_ROOTS)
    for (const root of roots) expect(root.parentSpanId).toBe('')
  }

  const expectStreams = () => {
    for (const request of server.requests) {
      expect(request.decodeError).toBeNull()
      expect(request.headers.authorization).toBe('Token token="relay-token"')
      for (const span of request.decoded.resourceSpans.flatMap((r) => r.scopeSpans.flatMap((s) => s.spans))) {
        expect(request.headers['buildkite-tests-span-stream']).toBe(span.name === 'test.execution' ? 'test' : 'child')
      }
    }
  }

  const childResources = () => server.requests
    .filter((r) => r.headers['buildkite-tests-span-stream'] === 'child')
    .flatMap((r) => r.decoded.resourceSpans.map((rs) => rs.resource))

  // Spans a suite's own exporter wrote to OTEL_SUITE_SPANS_FILE.
  const readSpansFile = (file) => (fs.existsSync(file)
    ? fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((line) => JSON.parse(line))
    : [])
  const spansFile = () => path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'otel-suite-')), 'spans.jsonl')
  const ownClientRequests = (recorded, port) => recorded.filter((s) => s.kind === 2 /* SDK CLIENT */ &&
    Object.values(s.attributes).some((value) => String(value) === port || String(value).includes(`:${port}/`)))

  // Children each test is expected to have under its own test span, in both
  // the collector-managed and suite-owned setups.
  const expectManualChildren = ({ rootsNamed, childrenOf, children }, { instrumented = true } = {}) => {
    for (const [test, child] of [
      ['concurrent slow', 'concurrent-slow'], ['concurrent fast', 'concurrent-fast'],
      ['concurrent plain slow', 'concurrent-plain-slow-start'], ['concurrent plain slow', 'concurrent-plain-slow'],
      ['concurrent plain fast', 'concurrent-plain-fast'],
      ['callback body', 'callback-child'], ['fake timers', 'fake-timer-child'],
      ['generator body', 'generator-child'], ['failing body', 'failing-child'],
    ]) {
      expect([test, childrenOf(rootsNamed(test)[0], named(child))]).toEqual([test, [expect.anything()]])
    }
    for (const test of ['http I/O', 'undici I/O', 'graphql resolver I/O', 'callback body']) {
      for (const hook of ['setup-child', 'teardown-child']) {
        expect([test, hook, childrenOf(rootsNamed(test)[0], named(hook)).length]).toEqual([test, hook, 1])
      }
    }
    // One test span per attempt, each with the children of its own attempt.
    const attempts = rootsNamed('retried I/O')
    expect(attempts.map((root) => root.attributes['test.case.result.status']).sort()).toEqual(['fail', 'pass'])
    for (const root of attempts) {
      const attempt = root.attributes['test.case.result.status'] === 'fail' ? 1 : 2
      expect(childrenOf(root, (s) => s.name.startsWith('attempt-')).map((s) => s.name)).toEqual([`attempt-${attempt}`])
      expect(childrenOf(root, clientGet)).toHaveLength(instrumented ? 1 : 0)
    }
    expect(children.filter(named('outside-test'))).toHaveLength(0)
  }

  test.each([
    ['in one process', '--runInBand'],
    ['in worker processes', '--maxWorkers=2 --no-cache'],
  ])('exports instrumented children under the test that ran them %s', async (_, args) => {
    // Batches of one export each span as it ends, so the collector's own
    // requests start inside running tests, with their instrumentation active.
    const outcome = await runChildren('managed', {
      args, env: { BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE: '1', BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE: '1' },
    })
    const { roots, children, rootsNamed, childrenOf, rootOf, isOwnRequest, warnings } = outcome
    expectAllResults(outcome)
    expectStreams()
    // owner.js registers its own provider without the Buildkite processor.
    expect(warnings).toEqual([expect.stringMatching(/the suite registers its own tracer provider, so add createBuildkiteSpanProcessor\(\)/)])

    for (const child of children) expect([child.name, rootOf(child) && roots.includes(rootOf(child))]).toEqual([child.name, true])
    expect(children.filter(isOwnRequest)).toEqual([])
    expectManualChildren(outcome)

    // Built-in modules are patched once per worker: one client span per
    // request in every file, and the in-process server continues the test's
    // trace.
    for (const test of ['http I/O', 'undici I/O', 'http again']) {
      const [client] = childrenOf(rootsNamed(test)[0], clientGet)
      expect([test, childrenOf(rootsNamed(test)[0], clientGet).length]).toEqual([test, 1])
      expect(childrenOf(rootsNamed(test)[0], (s) => s.kind === SERVER && s.parentSpanId === client.spanId)).toHaveLength(1)
    }
    const [manual] = childrenOf(rootsNamed('http I/O')[0], named('manual-parent'))
    expect(childrenOf(rootsNamed('http I/O')[0], clientGet)[0].parentSpanId).toBe(manual.spanId)

    // graphql is patched as the test file loads it; the resolver's fetch
    // nests under the resolver.
    const graphqlRoot = rootsNamed('graphql resolver I/O')[0]
    const [resolve] = childrenOf(graphqlRoot, named('graphql.resolve message'))
    expect(resolve).toBeDefined()
    expect(childrenOf(graphqlRoot, (s) => clientGet(s) && s.parentSpanId === resolve.spanId)).toHaveLength(1)

    // Express is patched under its main module's name, although the file it
    // re-exports loaded first: its spans nest under the in-process server.
    const expressRoot = rootsNamed('express route I/O')[0]
    const [expressServer] = childrenOf(expressRoot, (s) => s.kind === SERVER)
    const [handler] = childrenOf(expressRoot, (s) => s.attributes['express.type'] === 'request_handler')
    expect(handler).toEqual(expect.objectContaining({ name: 'request handler - /route', parentSpanId: expressServer.spanId }))
    expect(childrenOf(expressRoot, (s) => s.attributes['express.type'] === 'middleware').length).toBeGreaterThan(0)

    // node-redis is patched once its circularly required index.js has
    // finished loading, whether the file or the test loads it.
    for (const test of ['redis loaded with the file', 'redis loaded inside the test']) {
      expect([test, childrenOf(rootsNamed(test)[0], (s) => s.name.startsWith('redis-')).map((s) => s.name).sort()])
        .toEqual([test, ['redis-GET', 'redis-SET', 'redis-connect']])
    }

    // A file that owns its provider gets no forwarding and no instrumentation.
    expect(childrenOf(rootsNamed('own provider')[0])).toEqual([])
    expect(children.filter(named('owner-child'))).toEqual([])

    // Children carry the collector's resource, as test spans do.
    const testResource = server.requests.find((r) => r.headers['buildkite-tests-span-stream'] === 'test').decoded.resourceSpans[0].resource
    for (const resource of childResources()) expect(resource).toEqual(testResource)
  }, 60000)

  test.each(['suite', 'suite-off', 'suite-no-forwarder'])('leaves a provider the suite registers alone (%s)', async (mode) => {
    const file = spansFile()
    const outcome = await runChildren(mode, { env: { OTEL_SUITE_SPANS_FILE: file } })
    const { children, rootsNamed, childrenOf, isOwnRequest, warnings, port } = outcome
    expectAllResults(outcome)
    expectStreams()
    const recorded = readSpansFile(file)
    // The suite's own exporter never sees the collector's requests.
    expect(ownClientRequests(recorded, port)).toEqual([])
    expect(children.filter(isOwnRequest)).toEqual([])
    if (mode === 'suite-no-forwarder') {
      expect(warnings).toEqual([expect.stringMatching(/the suite registers its own tracer provider, so add createBuildkiteSpanProcessor\(\)/)])
    } else {
      expect(warnings).toEqual([])
    }
    if (mode !== 'suite') {
      expect(children).toEqual([])
      return
    }
    expectManualChildren(outcome)
    // Only the suite's instrumentation runs: its http, no graphql, and its
    // resource rather than the collector's.
    expect(children.filter((s) => s.name.startsWith('graphql.') || s.name.startsWith('redis-') || s.attributes['express.type'])).toEqual([])
    expect(childrenOf(rootsNamed('http I/O')[0], clientGet)).toHaveLength(1)
    expect(childrenOf(rootsNamed('own provider')[0], named('owner-child'))).toHaveLength(1)
    for (const resource of childResources()) expect(resource['service.name']).toBe('customer-service')
    // The suite's sampler and exporter still see everything it recorded.
    for (const child of children) expect(recorded.some((s) => s.name === child.name)).toBe(true)
  }, 60000)

  // Built-in modules stay patched for the worker's life. A later file that
  // opted out of the collector's environment gets no spans and no trace
  // headers, also when a require hook saw http before any instrumentation
  // registered for it.
  test('keeps its instrumentation out of a later file that opted out of the collector environment', async () => {
    const prime = path.join(__dirname, '../examples/jest/otel-children/prime.js')
    const outcome = await runChildren('optout', {
      env: { BUILDKITE_ANALYTICS_TOKEN: 'xyz', NODE_OPTIONS: `--require ${JSON.stringify(prime)}` },
    })
    const { result, roots, children, rootsNamed, childrenOf } = outcome
    expect([result.code, result.stderr]).toEqual([0, expect.not.stringMatching(/opted-out request carries no trace headers/)])
    expect(roots.map((root) => root.attributes['buildkite.test.name'])).not.toContain('opted-out request carries no trace headers')
    expect(jsonUpload(result.stdout).map((record) => [record.name, record.result])).toEqual([['opted-out request carries no trace headers', 'passed']])
    expect(childrenOf(rootsNamed('http I/O')[0], clientGet)).toHaveLength(1)
    for (const child of children) expect(roots).toContain(outcome.rootOf(child))
  }, 60000)

  test('leaves OpenTelemetry configured outside the test sandbox alone', async () => {
    const file = spansFile()
    const preload = path.join(__dirname, '../examples/jest/otel-children/preload.js')
    const outcome = await runChildren('preload', {
      args: '--maxWorkers=2 --no-cache',
      env: { OTEL_SUITE_SPANS_FILE: file, NODE_OPTIONS: `--require ${JSON.stringify(preload)}` },
    })
    expectAllResults(outcome)
    expect(outcome.warnings.filter((w) => !/registers its own tracer provider/.test(w))).toEqual([
      expect.stringMatching(/OpenTelemetry is configured outside the Jest test sandbox/),
    ])
    expect(outcome.children).toEqual([])
    const recorded = readSpansFile(file)
    expect(recorded.some((s) => s.name === 'GET')).toBe(true)
    expect(ownClientRequests(recorded, outcome.port)).toEqual([])
  }, 60000)

  test('exports test spans only, with one warning, without the collector runtime', async () => {
    const outcome = await runChildren('no-runtime', { args: '--maxWorkers=2 --no-cache' })
    expectAllResults(outcome)
    expect(outcome.warnings).toEqual([
      expect.stringMatching(/otelInstrumentations needs the collector's Jest runtime: set runtime: 'buildkite-test-collector\/jest\/runtime'/),
    ])
    expect(outcome.children).toEqual([])
  }, 60000)

  test('keeps manual children and test spans when otelInstrumentations is invalid', async () => {
    const outcome = await runChildren('bad-option')
    expectAllResults(outcome)
    expect(outcome.warnings.filter((w) => !/registers its own tracer provider/.test(w))).toEqual([
      expect.stringMatching(/otelInstrumentations is ignored: options for @opentelemetry\/instrumentation-http must be a plain JSON object/),
    ])
    expectManualChildren(outcome, { instrumented: false })
    expect(outcome.children.filter((s) => s.name === 'GET' || s.name.startsWith('graphql.'))).toEqual([])
  }, 60000)

  test('skips instrumentation that is missing, disabled in its options, or disabled with OTEL_NODE_DISABLED_INSTRUMENTATIONS', async () => {
    const outcome = await runChildren('missing-package', { env: { OTEL_NODE_DISABLED_INSTRUMENTATIONS: 'undici' } })
    const { rootsNamed, childrenOf, warnings } = outcome
    expectAllResults(outcome)
    expect(warnings.filter((w) => !/registers its own tracer provider/.test(w))).toEqual([
      expect.stringMatching(/OpenTelemetry instrumentation @opentelemetry\/instrumentation-not-installed could not be loaded .*continuing without it/),
    ])
    expect(childrenOf(rootsNamed('http I/O')[0], clientGet)).toHaveLength(1)
    expect(childrenOf(rootsNamed('undici I/O')[0], (s) => s.name === 'GET')).toEqual([])
    expect(outcome.children.filter((s) => s.name.startsWith('graphql.'))).toEqual([])
  }, 60000)

  test('overflows the bounded child queue without displacing test results or tracing its own exports', async () => {
    server = await startOtlpServer()
    const port = new URL(server.url).port
    const result = await run('jest --config otel-children.config.js io.js --runInBand', {
      ...baseEnv, OTEL_CHILD_CASE: 'flood',
      // Batches of one export from inside the tests that end the spans.
      BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE: '2', BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE: '1',
      BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE: '1',
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token', BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
    })
    expect(result.code).toBe(0)
    expect(result.stderr).toMatch(/OpenTelemetry dropped \d+ child span\(s\).*test.execution results are unaffected/)
    expect(result.stderr).not.toMatch(/TEST RESULTS MISSING/)
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    const spans = server.spans()
    expect(spans.filter((s) => s.name === 'test.execution')).toHaveLength(9) // io.js
    expect(spans.filter((s) => s.name === 'flood-child').length).toBeLessThan(100)
    expect(spans.filter((s) => s.name === 'GET').length).toBeGreaterThan(0)
    const own = spans.filter((s) => s.name.startsWith('POST') || Object.values(s.attributes).some((value) => String(value) === port))
    expect(own).toEqual([])
  }, 60000)

  test('stops child export with one warning per run when Buildkite refuses child spans', async () => {
    const outcome = await runChildren('managed', {
      args: '--maxWorkers=2 --no-cache',
      respond: (index, headers) => (headers['buildkite-tests-span-stream'] === 'child' ? 403 : 200),
    })
    expectAllResults(outcome)
    expect(outcome.warnings.filter((w) => !/registers its own tracer provider/.test(w))).toEqual([
      expect.stringMatching(/Buildkite Test Engine refused OpenTelemetry child spans \(HTTP 403\).*child span export has stopped\. test\.execution results are unaffected\./),
    ])
    const childRequests = server.requests.filter((r) => r.headers['buildkite-tests-span-stream'] === 'child')
    expect(childRequests.length).toBeGreaterThan(0)
    expect(childRequests.length).toBeLessThanOrEqual(2) // the first batch from each worker
  }, 60000)

  describe('with a relay token and a receiver that fails the first request', () => {
    let flakyServer
    let result
    beforeAll(async () => {
      flakyServer = await startOtlpServer({ respond: (index) => (index === 0 ? 503 : 200) })
      result = await run('jest --config otel.config.js', {
        ...baseEnv,
        BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
        BUILDKITE_ANALYTICS_OTLP_ENDPOINT: flakyServer.url,
      })
    }, 30000)
    afterAll(() => flakyServer.close())

    test('exports every test as a root test.execution span and uploads no JSON', () => {
      expect(result.code).toBe(1)
      expect(result.stdout).not.toMatch(/Test Engine Sending:/)
      expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]/)

      const spans = flakyServer.spans()
      const unique = new Map(spans.map((span) => [span.spanId, span]))
      expect(unique.size).toBe(EXPECTED_SPANS)

      for (const span of unique.values()) {
        expect(span.name).toBe('test.execution')
        expect(span.parentSpanId).toBe('')
        expect(span.attributes['buildkite.execution.via']).toBe('otlp')
        expect(span.attributes['buildkite.test.name']).not.toBe('')
        expect(span.attributes['buildkite.run_key']).toBe('otel-e2e-run')
        expect(['pass', 'fail', 'skipped']).toContain(span.attributes['test.case.result.status'])
        expect(span.startTimeUnixNano > 0n).toBe(true)
        expect(span.endTimeUnixNano >= span.startTimeUnixNano).toBe(true)
        expect(span.attributes['buildkite.tag.hello']).toBe('jest')
        expect(span.attributes['buildkite.tag.ci.worker.id']).toBe('agent-e2e')
        expect(span.attributes['buildkite.test.framework.name']).toBe('jest')
        expect(span.links).toEqual([expect.objectContaining({ traceId: '0af7651916cd43dd8448eb211c80319c' })])
      }
      const withLine = [...unique.values()].filter((span) => span.attributeTypes['code.line.number'] === 'int')
      expect(withLine).toHaveLength(EXPECTED_SPANS - 1)
      const [withoutLine] = [...unique.values()].filter((span) => !('code.line.number' in span.attributes))
      expect(withoutLine.attributes['buildkite.test.name']).toBe('throws a non-error value')
    })

    test('labels every request, including the retried one, with the run key and stream', () => {
      expect(flakyServer.requests.length).toBeGreaterThanOrEqual(2)
      expect(flakyServer.requests[0].status).toBe(503)
      for (const request of flakyServer.requests) {
        expect(request.headers).toMatchObject({
          authorization: 'Token token="relay-token"',
          'buildkite-tests-run-key': 'otel-e2e-run',
          'buildkite-tests-span-stream': 'test',
          'content-type': 'application/x-protobuf',
          'content-encoding': 'gzip',
        })
        expect(request.decodeError).toBeNull()
      }
    })

    test('derives the same identity and results as the JSON path', async () => {
      const json = await run('jest --config otel-off.config.js', { ...baseEnv, BUILDKITE_ANALYTICS_TOKEN: 'xyz' })
      const jsonRecords = jsonUpload(json.stdout)
      expect(jsonRecords).toHaveLength(EXPECTED_JSON_RESULTS)
      expect(json.code).toBe(result.code)

      const spanRecords = [...new Map(flakyServer.spans().map((span) => [span.spanId, span])).values()].map(spanRecord)
      expect(new Set(spanRecords.map(identity))).toEqual(new Set(jsonRecords.map(identity)))

      const RESULTS = { passed: 'pass', failed: 'fail', pending: 'skipped', skipped: 'skipped' }
      const retriedNames = ['passes on the second attempt', 'passes on the second describe attempt']
      for (const name of retriedNames) {
        expect(spanRecords.filter((record) => record.name === name).map((record) => record.result).sort()).toEqual(['fail', 'pass'])
      }
      const finalAttempts = spanRecords.filter((record) => !(retriedNames.includes(record.name) && record.result === 'fail'))
      expect(sortedBy(finalAttempts, identity)).toEqual(sortedBy(jsonRecords.map((record) => ({
        scope: record.scope,
        name: record.name,
        location: record.location,
        file_name: record.file_name,
        result: RESULTS[record.result],
        failure_reason: record.failure_reason,
      })), identity))
    }, 30000)
  })

  test.each([
    ['workers', '--maxWorkers=2'],
    ['workers with forceExit', '--maxWorkers=2 --forceExit'],
    ['in band', '--runInBand'],
    ['in band with forceExit', '--runInBand --forceExit'],
  ])('matches the final JSON result for a late rejection %s without extending its duration', async (_, flags) => {
    server = await startOtlpServer()
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'jest-late-errors-'))
    const outputFile = path.join(dir, 'results.json')
    const selection = `--testMatch '**/otel-late-errors/*.test.js' '**/passed.test.js' --no-cache --cacheDirectory ${dir} --json --outputFile ${outputFile} ${flags}`
    const env = { ...baseEnv, BUILDKITE_ANALYTICS_TOKEN: 'xyz', BUILDKITE_ANALYTICS_BASE_URL: 'http://127.0.0.1:1', BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url }
    try {
      const json = await run(`jest --config otel-off.config.js ${selection}`, env)
      const jsonRecords = jsonUpload(json.stdout)
      const jsonResult = JSON.parse(fs.readFileSync(outputFile, 'utf8'))
      const otel = await run(`jest --config otel.config.js ${selection}`, env)
      const otelResult = JSON.parse(fs.readFileSync(outputFile, 'utf8'))
      expect(json.code).toBe(1)
      expect(otel.code).toBe(json.code)
      expect(jsonRecords).toHaveLength(2)
      expect(otel.stdout).not.toMatch(/Test Engine Sending:/)
      expect(otel.stderr).not.toMatch(/\[buildkite-test-collector\]/)
      expect(otelResult.numTotalTests).toBe(2)
      expect(otelResult.numFailedTests).toBe(jestMajor >= 30 ? 1 : 0)
      const assertions = (result) => sortedBy(result.testResults.flatMap((file) => file.assertionResults).map(({ title, status, failureMessages }) => ({ title, status, failureMessages })), (test) => test.title)
      expect(assertions(otelResult)).toEqual(assertions(jsonResult))

      const spans = server.spans()
      expect(spans).toHaveLength(2)
      const late = spans.filter((span) => span.attributes['buildkite.test.name'] === 'rejects after test_done during afterAll')
      expect(late).toHaveLength(1)
      const [span] = late
      const record = jsonRecords.find((record) => record.name === 'rejects after test_done during afterAll')
      expect(record.result).toBe(jestMajor >= 30 ? 'failed' : 'passed')
      expect(spanRecord(span)).toEqual({
        scope: record.scope, name: record.name, location: record.location, file_name: record.file_name,
        result: record.result === 'failed' ? 'fail' : 'pass', failure_reason: record.failure_reason,
      })
      expect(span.status.code).toBe(record.result === 'failed' ? 2 : 0)
      expect(span.events).toEqual(record.result === 'failed' ? [{
        name: 'exception', attributes: { 'exception.message': record.failure_expanded[0].expanded.join('\n') },
      }] : [])
      const afterAllStarted = Number(otel.stdout.match(/afterAll started at (\d+)/)[1])
      expect(span.endTimeUnixNano <= BigInt(afterAllStarted) * 1000000n).toBe(true)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  }, 30000)

  test('ignores OTEL_* exporter, sampler and span-limit configuration', async () => {
    server = await startOtlpServer()
    const result = await run('jest --config otel.config.js', {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      OTEL_EXPORTER_OTLP_HEADERS: 'authorization=garbage,x-env-only=1',
      OTEL_EXPORTER_OTLP_TRACES_HEADERS: 'authorization=garbage2,x-env-traces=1',
      OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: 'http/json',
      OTEL_EXPORTER_OTLP_ENDPOINT: 'http://127.0.0.1:9/v1/traces',
      OTEL_TRACES_SAMPLER: 'always_off',
      OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT: '2',
    })

    expect(result.code).toBe(1)
    expect(server.spans()).toHaveLength(EXPECTED_SPANS)
    for (const request of server.requests) {
      expect(request.headers.authorization).toBe('Token token="relay-token"')
      expect(request.headers['content-type']).toBe('application/x-protobuf')
      expect(request.headers).not.toHaveProperty('x-env-only')
      expect(request.headers).not.toHaveProperty('x-env-traces')
    }
    for (const span of server.spans()) {
      expect(span.droppedAttributesCount).toBe(0)
      expect(span.attributes['buildkite.tag.hello']).toBe('jest')
    }
  }, 30000)

  test('uploads JSON silently when otelEnabled is set but no token is available', async () => {
    server = await startOtlpServer()
    const result = await run('jest --config otel.config.js', { ...baseEnv, BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url })

    expect(result.code).toBe(1)
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]/)
    expect(result.stderr).toMatch(/Missing BUILDKITE_ANALYTICS_TOKEN/)
    expect(server.requests).toHaveLength(0)
  }, 30000)

  test('warns and uploads JSON when the run key is invalid', async () => {
    server = await startOtlpServer()
    const result = await run('jest --config otel.config.js', {
      ...baseEnv,
      BUILDKITE_ANALYTICS_TOKEN: 'xyz',
      BUILDKITE_ANALYTICS_KEY: 'has a space',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
    })

    expect(result.code).toBe(1)
    expect(result.stderr).toMatch(/OpenTelemetry span export disabled: run key 'has a space' is invalid/)
    expect(result.stderr).toMatch(/uploading results as JSON instead/)
    expect(jsonUpload(result.stdout)).toHaveLength(EXPECTED_JSON_RESULTS)
    expect(server.requests).toHaveLength(0)
  }, 30000)

  test('warns and uploads JSON for files that did not run under the collector environment', async () => {
    server = await startOtlpServer()
    const result = await run('jest --config otel.config.js --testEnvironment node', {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_TOKEN: 'xyz',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
    })

    expect(result.code).toBe(1)
    expect(result.stderr.match(/is not buildkite-test-collector\/jest\/environment/g)).toHaveLength(1)
    expect(result.stderr).toMatch(/for test files using it, uploading results as JSON instead\./)
    expect(jsonUpload(result.stdout)).toHaveLength(EXPECTED_JSON_RESULTS)
    expect(server.requests).toHaveLength(0)
  }, 30000)

  test('says results will not be uploaded when the fallback has no JSON token', async () => {
    server = await startOtlpServer()
    const result = await run('jest --config otel.config.js --testEnvironment node', {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
    })

    expect(result.code).toBe(1)
    expect(result.stderr).toMatch(/is not buildkite-test-collector\/jest\/environment .*for test files using it, results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set\./)
    expect(result.stderr).not.toMatch(/as JSON/)
    expect(jsonUpload(result.stdout)).toBeNull()
    expect(server.requests).toHaveLength(0)
  }, 30000)

  describe('with files that choose their environment through @jest-environment', () => {
    const spanNames = () => server.spans().map((span) => span.attributes['buildkite.test.name']).sort()
    const jsonNames = (stdout) => (jsonUpload(stdout) || []).map((record) => record.name).sort()

    test('uploads the file that opted out of the collector environment as JSON, the rest over OTLP', async () => {
      server = await startOtlpServer()
      const result = await run('jest --config otel-docblock.config.js', {
        ...baseEnv,
        BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
        BUILDKITE_ANALYTICS_TOKEN: 'xyz',
        BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      })

      expect(result.code).toBe(0)
      expect(result.stderr.match(/is not buildkite-test-collector\/jest\/environment/g)).toHaveLength(1)
      expect(spanNames()).toEqual(['runs under the collector environment', 'runs under the default environment'])
      expect(jsonNames(result.stdout)).toEqual(['runs under the node environment'])
    }, 30000)

    test('exports only the file that opted in when the default environment is node', async () => {
      server = await startOtlpServer()
      const result = await run('jest --config otel-docblock.config.js --testEnvironment node', {
        ...baseEnv,
        BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
        BUILDKITE_ANALYTICS_TOKEN: 'xyz',
        BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      })

      expect(result.code).toBe(0)
      expect(result.stderr.match(/is not buildkite-test-collector\/jest\/environment/g)).toHaveLength(1)
      expect(spanNames()).toEqual(['runs under the collector environment'])
      expect(jsonNames(result.stdout)).toEqual(['runs under the default environment', 'runs under the node environment'])
    }, 30000)
  })

  test('exports once, and uploads no JSON, when the environment only loads through the project transformer', async () => {
    server = await startOtlpServer()
    // --no-cache keeps the environment out of the main process's module
    // cache, so the reporter has to load it the way the workers do.
    const result = await run('jest --config otel-transformed.config.js --no-cache --maxWorkers=2', {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_TOKEN: 'xyz',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
    })

    expect(result.code).toBe(0)
    expect(result.stderr).not.toMatch(/is not buildkite-test-collector\/jest\/environment/)
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    expect(server.spans().map((span) => span.attributes['buildkite.test.name']).sort()).toEqual([
      'runs under the transformed environment (first file)',
      'runs under the transformed environment (second file)',
    ])
  }, 30000)

  test('sends each test once when the reporter cannot load the environment its workers ran', async () => {
    server = await startOtlpServer()
    const cacheDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'jest-cache-'))
    // A fresh cache has no timings, so Jest runs the two files in worker processes.
    const result = await run(`jest --config otel-worker-only.config.js --maxWorkers=2 --cacheDirectory ${cacheDirectory}`, {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_TOKEN: 'xyz',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
    })
    fs.rmSync(cacheDirectory, { recursive: true, force: true })

    expect(result.code).toBe(0)
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    expect(server.spans().map((span) => span.attributes['buildkite.test.name']).sort()).toEqual([
      'runs in a worker (first file)',
      'runs in a worker (second file)',
    ])
  }, 30000)

  test('warns TEST RESULTS MISSING and keeps a passing exit code when the endpoint is unreachable', async () => {
    const closed = await startOtlpServer()
    await closed.close()
    const result = await run('jest --config otel.config.js passed.test.js', {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_TOKEN: 'xyz',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: closed.url,
    })

    expect(result.code).toBe(0)
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    expect(result.stderr).toMatch(/TEST RESULTS MISSING: OpenTelemetry dropped 1 test\.execution span\(s\) \(export-failure: 1, last OTLP failure: ECONNREFUSED\)/)
  }, 90000)

  test('accounts for every span when the endpoint refuses connections for longer than the flush budget', async () => {
    const closed = await startOtlpServer()
    await closed.close()
    const cacheDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'jest-cache-'))
    // Each worker spends its 30 s budget on the retries of its first files;
    // Jest then force-exits the workers, whose later exports never finish.
    const result = await run(`jest --config otel-many-files.config.js --maxWorkers=2 --cacheDirectory ${cacheDirectory}`, {
      ...baseEnv,
      BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
      BUILDKITE_ANALYTICS_TOKEN: 'xyz',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: closed.url,
    })
    fs.rmSync(cacheDirectory, { recursive: true, force: true })

    const total = (pattern) => [...result.stderr.matchAll(pattern)].reduce((sum, match) => sum + Number(match[1]), 0)
    const dropped = total(/TEST RESULTS MISSING: OpenTelemetry dropped (\d+) test\.execution span\(s\) \(export-failure: \1, last OTLP failure: ECONNREFUSED\)/g)
    const pending = total(/TEST RESULTS MISSING: (\d+) test\.execution span\(s\) were still exporting/g)
    expect(result.code).toBe(0)
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    expect(dropped + pending).toBe(30)
  }, 90000)

  test('does not load the OpenTelemetry SDK when otelEnabled is off', async () => {
    const script = [
      'const Reporter = require("buildkite-test-collector/jest/reporter")',
      'new Reporter({ rootDir: process.cwd() }, { tags: {} })',
      'console.log(JSON.stringify(Object.keys(require.cache).filter((key) => key.includes("@opentelemetry"))))',
    ].join('; ')
    const stdout = await new Promise((resolve, reject) => {
      const cwd = path.join(__dirname, '../examples/jest')
      execFile(process.execPath, ['-e', script], { cwd, env: { ...process.env, ...baseEnv, BUILDKITE_ANALYTICS_TOKEN: 'xyz' } }, (error, out) => (error ? reject(error) : resolve(out)))
    })

    expect(JSON.parse(stdout.trim())).toEqual([])
  })
})
