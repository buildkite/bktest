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

// OpenTelemetry submission needs Jest 29 or newer; older versions upload JSON.
const otelSupported = jestMajor >= 29

;(otelSupported ? test.skip : test)('warns once and uploads JSON on Jest 28 and older', async () => {
  server = await startOtlpServer()
  const result = await run('jest --config otel-children.config.js', {
    ...baseEnv, BUILDKITE_ANALYTICS_TOKEN: 'xyz', BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token', BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
  })
  expect(result.stderr.split('\n').filter((line) => line.includes('[buildkite-test-collector]'))).toEqual([
    `[buildkite-test-collector] otelEnabled is set, but OpenTelemetry submission needs Jest 29 or newer, not ${jestVersion}; uploading results as JSON instead`,
  ])
  expect(jsonUpload(result.stdout)).toHaveLength(20) // every test in otel-children
  expect(server.requests).toHaveLength(0)
}, 60000)

;(otelSupported ? describe : describe.skip)('examples/jest in OTel mode', () => {
  // examples/jest/otel-children.config.js: 20 tests in io.js, again.js,
  // empty.js and owner.js; again.js retries one test, which fails once.
  const CHILD_FIXTURE_ROOTS = 21
  const CLIENT = 3
  const SERVER = 2
  const SUITE_PROVIDER_WARNING = /OpenTelemetry child spans are not exported because the suite registers its own tracer provider; to get them, let the collector provide the provider under Jest\./
  const LATE_WARNING = /OpenTelemetry dropped 1 child span\(s\) \(still open when their test file finished: 1\); test\.execution results are unaffected\./

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

  // io.js alone has 7 tests, which pass.
  const IO_ROOTS = 7
  const expectAllResults = ({ result, roots }, count = CHILD_FIXTURE_ROOTS) => {
    expect(result.code).toBe(count === IO_ROOTS ? 0 : 1) // intentional failures in empty.js
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    expect(result.stderr).not.toMatch(/TEST RESULTS MISSING/)
    expect(roots).toHaveLength(count)
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

  // Spans the preloaded provider wrote to OTEL_SUITE_SPANS_FILE.
  const readSpansFile = (file) => (fs.existsSync(file)
    ? fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((line) => JSON.parse(line))
    : [])
  const spansFile = () => path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'otel-suite-')), 'spans.jsonl')
  const ownClientRequests = (recorded, port) => recorded.filter((s) => s.kind === 2 /* SDK CLIENT */ &&
    Object.values(s.attributes).some((value) => String(value) === port || String(value).includes(`:${port}/`)))

  // Manual children each test is expected to have under its own test span.
  const expectManualChildren = ({ rootsNamed, childrenOf, children }, { instrumented = true } = {}) => {
    for (const [test, child] of [
      ['concurrent slow', 'concurrent-slow'], ['concurrent fast', 'concurrent-fast'],
      ['concurrent plain slow', 'concurrent-plain-slow-start'], ['concurrent plain slow', 'concurrent-plain-slow'],
      ['concurrent plain fast', 'concurrent-plain-fast'],
      ['callback body', 'callback-child'], ['fake timers', 'fake-timer-child'],
      ['generator body', 'generator-child'], ['failing body', 'failing-child'],
      ['after jest.resetModules', 'reset-modules-child'], ['in jest.isolateModules', 'isolated-modules-child'],
    ]) {
      expect([test, childrenOf(rootsNamed(test)[0], named(child))]).toEqual([test, [expect.anything()]])
    }
    // The file's own hooks and those of a setupFilesAfterEnv file, also in a
    // file that resets its modules before the collector sets up (again.js).
    for (const test of ['http I/O', 'undici I/O', 'callback body', 'http again']) {
      const hooks = test === 'http again' ? ['setup-file-before', 'setup-file-after'] : ['setup-child', 'teardown-child', 'setup-file-before', 'setup-file-after']
      for (const hook of hooks) {
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
    // beforeAll spans start traces of their own.
    expect(children.filter(named('outside-test'))).toHaveLength(0)
  }

  // A request to the fixture app nests client GET > server GET > the
  // handler's manual span under the test that made it.
  const expectRequest = ({ rootsNamed, childrenOf }, test) => {
    const root = rootsNamed(test)[0]
    const clients = childrenOf(root, clientGet)
    const servers = childrenOf(root, (s) => s.kind === SERVER && s.parentSpanId === clients[0]?.spanId)
    const handlers = childrenOf(root, (s) => s.name === 'handle-request' && s.parentSpanId === servers[0]?.spanId)
    expect([test, clients.length, servers.length, handlers.length]).toEqual([test, 1, 1, 1])
  }

  test.each([
    ['in one process', '--runInBand'],
    ['in worker processes', '--maxWorkers=2 --no-cache'],
  ])('exports children under the test that ran them %s', async (_, args) => {
    // Batches of one export each span as it ends, so the collector's own
    // requests start inside running tests, with their instrumentation active.
    const outcome = await runChildren('managed', {
      args, env: { BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE: '1', BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE: '1' },
    })
    const { roots, children, rootsNamed, childrenOf, rootOf, isOwnRequest, warnings } = outcome
    expectAllResults(outcome)
    expectStreams()
    // owner.js registers its own provider; again.js leaves a child open.
    expect(warnings.sort()).toEqual([expect.stringMatching(SUITE_PROVIDER_WARNING), expect.stringMatching(LATE_WARNING)])
    // Each patch is undone exactly once: shimmer complains about a second unwrap.
    expect(outcome.result.stderr).not.toMatch(/already been unwrapped/)

    for (const child of children) expect([child.name, rootOf(child) && roots.includes(rootOf(child))]).toEqual([child.name, true])
    expect(children.filter(isOwnRequest)).toEqual([])
    expectManualChildren(outcome)

    // Built-in modules are patched once per file, also in the second file of
    // a worker: one client span per request, and the in-process server
    // continues the test's trace.
    for (const test of ['http I/O', 'undici I/O', 'http again']) expectRequest(outcome, test)
    const [manual] = childrenOf(rootsNamed('http I/O')[0], named('manual-parent'))
    expect(childrenOf(rootsNamed('http I/O')[0], clientGet)[0].parentSpanId).toBe(manual.spanId)

    // A file that owns its provider gets no forwarding and no instrumentation.
    expect(childrenOf(rootsNamed('own provider')[0])).toEqual([])
    expect(children.filter(named('owner-child'))).toEqual([])
    expect(children.filter(named('late-child'))).toEqual([])

    // Children carry the collector's resource, as test spans do.
    const testResource = server.requests.find((r) => r.headers['buildkite-tests-span-stream'] === 'test').decoded.resourceSpans[0].resource
    for (const resource of childResources()) expect(resource).toEqual(testResource)
  }, 60000)

  // Built-in modules are unpatched at each file's teardown. A later file that
  // opted out of the collector's environment finds no wrappers and sends no
  // trace headers, also when a require hook saw http before any
  // instrumentation registered for it, so the collector patched it itself.
  test('unpatches built-ins before a later file that opted out of the collector environment', async () => {
    const prime = path.join(__dirname, '../examples/jest/otel-children/support/prime.js')
    const outcome = await runChildren('optout', {
      env: { BUILDKITE_ANALYTICS_TOKEN: 'xyz', NODE_OPTIONS: `--require ${JSON.stringify(prime)}` },
    })
    const { result, roots, children, rootsNamed, childrenOf } = outcome
    expect([result.code, result.stderr]).toEqual([0, expect.not.stringMatching(/opted-out request carries no trace headers|already been unwrapped/)])
    expect(roots.map((root) => root.attributes['buildkite.test.name'])).not.toContain('opted-out request carries no trace headers')
    expect(jsonUpload(result.stdout).map((record) => [record.name, record.result])).toEqual([['opted-out request carries no trace headers', 'passed']])
    expect(childrenOf(rootsNamed('http I/O')[0], clientGet)).toHaveLength(1)
    for (const child of children) expect(roots).toContain(outcome.rootOf(child))
  }, 60000)

  test('leaves OpenTelemetry configured outside the test sandbox alone, with one warning', async () => {
    const file = spansFile()
    const preload = path.join(__dirname, '../examples/jest/otel-children/support/preload.js')
    const outcome = await runChildren('preload', {
      args: '--maxWorkers=2 --no-cache',
      env: { OTEL_SUITE_SPANS_FILE: file, NODE_OPTIONS: `--require ${JSON.stringify(preload)}` },
    })
    expectAllResults(outcome)
    expect(outcome.warnings).toEqual([expect.stringMatching(SUITE_PROVIDER_WARNING)])
    expect(outcome.children).toEqual([])
    const recorded = readSpansFile(file)
    expect(recorded.some((s) => s.name === 'GET')).toBe(true)
    // The preloaded instrumentation never records the collector's requests.
    expect(ownClientRequests(recorded, outcome.port)).toEqual([])
  }, 60000)

  test('exports test spans only, without a warning, without the collector runtime', async () => {
    const outcome = await runChildren('no-runtime', { args: '--maxWorkers=2 --no-cache' })
    expectAllResults(outcome)
    expect(outcome.warnings).toEqual([])
    expect(outcome.children).toEqual([])
  }, 60000)

  // As @opentelemetry/auto-instrumentations-node reads them: short names, the
  // enabled list first, then the disabled list.
  test.each([
    ['OTEL_NODE_ENABLED_INSTRUMENTATIONS', { OTEL_NODE_ENABLED_INSTRUMENTATIONS: 'undici' }, { 'http I/O': 0, 'undici I/O': 1 }],
    ['both variables', { OTEL_NODE_ENABLED_INSTRUMENTATIONS: ' http , undici', OTEL_NODE_DISABLED_INSTRUMENTATIONS: 'undici' }, { 'http I/O': 1, 'undici I/O': 0 }],
  ])('narrows the instrumentation with %s', async (_, env, clients) => {
    const outcome = await runChildren('managed', { args: 'io.js --runInBand', env })
    expectAllResults(outcome, IO_ROOTS)
    expect(outcome.warnings).toEqual([])
    for (const [test, count] of Object.entries(clients)) {
      expect([test, outcome.childrenOf(outcome.rootsNamed(test)[0], clientGet).length]).toEqual([test, count])
    }
    expect(outcome.childrenOf(outcome.rootsNamed('callback body')[0], named('callback-child'))).toHaveLength(1)
  }, 60000)

  // A project that can load the instrumentation only through
  // @opentelemetry/auto-instrumentations-node, as under pnpm, which keeps a
  // package's dependencies beside it rather than in the project's node_modules.
  const projectWithAutoInstrumentations = (packages) => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-auto-'))
    const auto = path.join(root, 'node_modules/@opentelemetry/auto-instrumentations-node')
    fs.mkdirSync(path.join(auto, 'node_modules/@opentelemetry'), { recursive: true })
    fs.writeFileSync(path.join(auto, 'package.json'), JSON.stringify({ name: '@opentelemetry/auto-instrumentations-node', version: '0.0.0', main: 'index.js' }))
    fs.writeFileSync(path.join(auto, 'index.js'), '')
    for (const [name, source] of Object.entries(packages)) {
      const target = path.join(auto, 'node_modules', name)
      if (typeof source === 'string') {
        fs.symlinkSync(source, target)
      } else {
        // A stand-in for an instrumentation release from another SDK line.
        fs.mkdirSync(target)
        fs.writeFileSync(path.join(target, 'package.json'), JSON.stringify({ name, version: source.version, main: 'index.js' }))
        fs.writeFileSync(path.join(target, 'index.js'), `exports.${source.className} = class { constructor() { this.instrumentationVersion = '${source.version}' } }`)
      }
    }
    return root
  }
  const installed = (name) => path.dirname(require.resolve(`${name}/package.json`, { paths: [path.join(__dirname, '../examples/jest')] }))

  test.each([
    ['records http and fetch with the 2.x SDK line', {
      '@opentelemetry/instrumentation-http': installed('@opentelemetry/instrumentation-http'),
      '@opentelemetry/instrumentation-undici': installed('@opentelemetry/instrumentation-undici'),
    }],
    ['skips another SDK line with one warning', {
      '@opentelemetry/instrumentation-http': { version: '0.300.0', className: 'HttpInstrumentation' },
      '@opentelemetry/instrumentation-undici': { version: '0.34.0', className: 'UndiciInstrumentation' },
    }],
  ])('with only @opentelemetry/auto-instrumentations-node installed, %s', async (_, packages) => {
    const root = projectWithAutoInstrumentations(packages)
    try {
      const outcome = await runChildren('managed', { args: 'io.js --maxWorkers=2 --no-cache', env: { OTEL_PROJECT_ROOT: root } })
      expectAllResults(outcome, IO_ROOTS)
      expect(outcome.childrenOf(outcome.rootsNamed('callback body')[0], named('callback-child'))).toHaveLength(1)
      if (typeof packages['@opentelemetry/instrumentation-http'] === 'string') {
        expect(outcome.warnings).toEqual([])
        for (const test of ['http I/O', 'undici I/O']) expectRequest(outcome, test)
      } else {
        expect(outcome.warnings).toEqual([expect.stringMatching(
          /OpenTelemetry instrumentation skipped: @opentelemetry\/instrumentation-http 0\.300\.0, @opentelemetry\/instrumentation-undici 0\.34\.0\. .*2\.x SDK line.*test\.execution export remains enabled\./,
        )])
        expect(outcome.children.filter((s) => s.name === 'GET')).toEqual([])
      }
    } finally {
      fs.rmSync(root, { recursive: true, force: true })
    }
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
    expect(result.stderr).toMatch(/OpenTelemetry dropped \d+ child span\(s\) \(not-exported: \d+\); test.execution results are unaffected/)
    expect(result.stderr).not.toMatch(/TEST RESULTS MISSING/)
    expect(result.stdout).not.toMatch(/Test Engine Sending:/)
    const spans = server.spans()
    expect(spans.filter((s) => s.name === 'test.execution')).toHaveLength(7) // io.js
    expect(spans.filter((s) => s.name === 'flood-child').length).toBeLessThan(100)
    expect(spans.filter((s) => s.name === 'GET').length).toBeGreaterThan(0)
    const own = spans.filter((s) => s.name.startsWith('POST') || Object.values(s.attributes).some((value) => String(value) === port))
    expect(own).toEqual([])
  }, 60000)

  // As in Ruby: a refused batch counts as dropped, the first drop warns, the
  // process warns its total when it finishes, and the stream keeps sending.
  test('counts child batches Buildkite refuses with HTTP 403 as dropped and keeps sending', async () => {
    const outcome = await runChildren('managed', {
      env: { BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE: '1' },
      respond: (index, headers) => (headers['buildkite-tests-span-stream'] === 'child' ? 403 : 200),
    })
    expectAllResults(outcome)
    const childRequests = server.requests.filter((r) => r.headers['buildkite-tests-span-stream'] === 'child')
    const refused = childRequests.flatMap((r) => r.decoded.resourceSpans.flatMap((rs) => rs.scopeSpans.flatMap((ss) => ss.spans)))
    // Requests continue through every file that has children.
    expect(refused.map((s) => s.name)).toEqual(expect.arrayContaining(['manual-parent', 'reset-modules-child']))
    expect(childRequests.every((r) => r.status === 403)).toBe(true)
    expect(outcome.warnings.filter((w) => !SUITE_PROVIDER_WARNING.test(w))).toEqual([
      expect.stringMatching(/OpenTelemetry dropped \d+ child span\(s\) \(export-failure: \d+, last OTLP failure: 403(, still open when their test file finished: 1)?\); test\.execution results are unaffected\./),
      `[buildkite-test-collector] OpenTelemetry dropped ${refused.length + 1} child span(s) so far this run ` +
        `(export-failure: ${refused.length}, last OTLP failure: 403, still open when their test file finished: 1); test.execution results are unaffected.`,
    ])
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
