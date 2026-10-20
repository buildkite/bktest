const { exec, execFile } = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')
const { startOtlpServer } = require('./support/otlpServer')

// examples/jest/otel.config.js runs: example.test.js (2), passed.test.js (1),
// otel/outcomes.js (9) and otel/retries.js (1 test, 2 attempts).
const EXPECTED_SPANS = 14
const EXPECTED_JSON_RESULTS = 13
const jestMajor = Number(require(require.resolve('jest/package.json', { paths: [path.join(__dirname, '../examples/jest')] })).version.split('.')[0])

const run = (command, env) => new Promise((resolve) => {
  const cwd = path.join(__dirname, '../examples/jest')
  exec(command, { cwd, env: { ...process.env, ...env } }, (error, stdout, stderr) => {
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
      const finalAttempts = spanRecords.filter((record) => !(record.name === 'passes on the second attempt' && record.result === 'fail'))
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
