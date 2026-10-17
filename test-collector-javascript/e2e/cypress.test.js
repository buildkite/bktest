// Does an end-to-end test of the Mocha example, using the debug output from the
// reporter, and verifying the JSON
require('dotenv').config();
const { exec, execFile } = require('child_process');
const { hasUncaughtExceptionCaptureCallback } = require('process');
const path = require('path');
const { startOtlpServer } = require('./support/otlpServer')

const DEFAULT_TIMEOUT = 20000 // 20s timeout

describe('examples/cypress', () => {
  const cwd = path.join(__dirname, "../examples/cypress")
  const env = {
    ...process.env,
    BUILDKITE_ANALYTICS_TOKEN: "xyz",
    BUILDKITE_ANALYTICS_CYPRESS_TOKEN: "abc",
    BUILDKITE_ANALYTICS_DEBUG_ENABLED: "true",
  }

  describe('when token is defined through config', () => {
    test('it uses the correct token', (done) => {
      exec('npm test -- --config-file cypress.config.token.js',
        { cwd, env: { ...env, BUILDKITE_ANALYTICS_TOKEN: undefined } }, (error, stdout, stderr) => {
          expect(stdout).toMatch(/.*Test Engine Sending: ({.*})/m);

          const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
          const json = JSON.parse(jsonMatch[1])["headers"]

          expect(json).toHaveProperty("Authorization", 'Token token="abc"')

          done()
        })
    }, DEFAULT_TIMEOUT)
  })

  describe('when token is defined through BUILDKITE_ANALYTICS_TOKEN', () => {
    test('it uses the correct token', (done) => {
      exec('npm test', { cwd, env }, (error, stdout, stderr) => {
        expect(stdout).toMatch(/.*Test Engine Sending: ({.*})/m);

        const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
        const json = JSON.parse(jsonMatch[1])["headers"]

        expect(json).toHaveProperty("Authorization", 'Token token="xyz"')

        done()
      })
    }, DEFAULT_TIMEOUT)
  })

  test('it posts the correct JSON', (done) => {
    exec('npm test', { cwd, env }, (error, stdout, stderr) => {
      expect(stdout).toMatch(/.*Test Engine Sending: ({.*})/m);

      const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
      const json = JSON.parse(jsonMatch[1])["data"]

      // Uncomment to view the JSON
      // console.log(json)

      expect(json).toHaveProperty("format", "json")

      expect(json).toHaveProperty("run_env.ci")
      expect(json).toHaveProperty("run_env.debug", 'true')
      expect(json).toHaveProperty("run_env.key")
      expect(json).toHaveProperty("run_env.version")
      expect(json).toHaveProperty("run_env.collector", "js-buildkite-test-collector")
      expect(json).toHaveProperty("run_env.test_runner", "cypress")

      expect(json).toHaveProperty("tags", { "hello": "cypress" }) // examples/cypress/cypress.config.js

      expect(json).toHaveProperty("data[0].name", 'renders')
      expect(json).toHaveProperty("data[0].identifier", '<Component /> renders')
      expect(json).toHaveProperty("data[0].location", "cypress/component/Component.cy.jsx") // Cypress does not report test line numbers, otherwise we'd see it here
      expect(json).toHaveProperty("data[0].file_name", "cypress/component/Component.cy.jsx")
      expect(json).toHaveProperty("data[0].result", 'passed')

      expect(json).toHaveProperty("data[1].scope", "<Component />")
      expect(json).toHaveProperty("data[1].name", "fails")
      expect(json).toHaveProperty("data[1].identifier", "<Component /> fails")
      expect(json).toHaveProperty("data[1].location", "cypress/component/Component.cy.jsx")  // Cypress does not report test line numbers, otherwise we'd see it here
      expect(json).toHaveProperty("data[1].file_name", "cypress/component/Component.cy.jsx")
      expect(json).toHaveProperty("data[1].result", "failed")
      expect(json).toHaveProperty("data[1].failure_reason")
      expect(json.data[1].failure_reason).toMatch("expected '<label#label>' to have text 'Label2', but the text was 'Label'")
      expect(json).toHaveProperty("data[1].failure_expanded[0].expanded")
      expect(json).toHaveProperty("data[1].failure_expanded[0].backtrace")

      expect(stdout).toMatch(/Test Engine .* response/m)
      done()
    })
  }, DEFAULT_TIMEOUT)

  test('it supports test location prefixes for monorepos', (done) => {
    exec('npm test', { cwd, env: { ...env, BUILDKITE_ANALYTICS_LOCATION_PREFIX: "some-sub-dir/" } }, (error, stdout, stderr) => {
      expect(stdout).toMatch(/.*Test Engine Sending: ({.*})/m);

      const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
      const json = JSON.parse(jsonMatch[1])["data"]

      // Uncomment to view the JSON
      // console.log(json)

      expect(json).toHaveProperty("run_env.location_prefix", "some-sub-dir/")

      expect(json).toHaveProperty("data[0].location", "some-sub-dir/cypress/component/Component.cy.jsx")
      expect(json).toHaveProperty("data[1].location", "some-sub-dir/cypress/component/Component.cy.jsx")

      done()
    })
  }, DEFAULT_TIMEOUT)

  describe('when test is pass but upload fails', () => {
    beforeAll(() => {
      // This will cause the upload to fail
      env.BUILDKITE_ANALYTICS_BASE_URL = "http://"
    })

    test('it should not throw an error', (done) => {
      exec('npm test -- --spec cypress/component/passed.cy.js', { cwd, env }, (error, stdout, stderr) => {
        expect(error).toBeNull()

        done()
      })
    }, DEFAULT_TIMEOUT)
  })
})

describe('examples/cypress in OTel mode', () => {
  const run = (env, args = '') => new Promise((resolve) => {
    exec(`npx cypress run --config-file otel.config.js ${args}`, {
      cwd: path.join(__dirname, '../examples/cypress'),
      env: {
        ...process.env,
        BUILDKITE_ANALYTICS_DEBUG_ENABLED: 'true',
        BUILDKITE_ANALYTICS_TOKEN: undefined,
        BUILDKITE_ANALYTICS_CYPRESS_TOKEN: undefined,
        BUILDKITE_TESTS_OTLP_TOKEN: undefined,
        BUILDKITE_ANALYTICS_KEY: 'cypress-otel-e2e',
        BUILDKITE_AGENT_ID: 'cypress-agent',
        ...env,
      },
    }, (error, stdout, stderr) => resolve({ code: error ? error.code : 0, stdout, stderr }))
  })
  const identity = (record) => JSON.stringify([record.scope, record.name, record.file_name])
  const records = (stdout) => {
    const uploads = [...stdout.matchAll(/Test Engine Sending: ({.*})/g)].map((m) => JSON.parse(m[1]).data.data)
    return [...new Map(uploads.flat().map((record) => [record.id, record])).values()]
  }
  let server
  afterEach(async () => { if (server) await server.close() })

  test.each([false, true])('keeps JSON available with an absent SDK (otelEnabled=%s)', async (enabled) => {
    server = await startOtlpServer()
    const script = `
      const Module = require('module')
      const load = Module._load
      let attempted = false
      Module._load = function (name, ...args) {
        if (name.startsWith('@opentelemetry/')) {
          attempted = true
          throw new Error('SDK intentionally unavailable')
        }
        return load.call(this, name, ...args)
      }
      const Mocha = require('mocha')
      const Reporter = require('./cypress/reporter')
      const suite = new Mocha.Suite('example')
      const test = new Mocha.Test('passes', () => {})
      suite.addTest(test)
      test.file = 'example.cy.js'
      test.state = 'passed'
      const runner = new Mocha.Runner(suite)
      const reporter = new Reporter(runner, { reporterOptions: { otelEnabled: ${enabled} } })
      runner.emit('test', test)
      runner.emit('test end', test)
      reporter.done(0, () => console.log('SDK_ATTEMPTED=' + attempted))
    `
    const { stdout, stderr } = await new Promise((resolve, reject) => {
      execFile(process.execPath, ['-e', script], {
        cwd: path.join(__dirname, '..'),
        env: { ...process.env, BUILDKITE_ANALYTICS_TOKEN: 'json-token', BUILDKITE_TESTS_OTLP_TOKEN: '',
          BUILDKITE_ANALYTICS_BASE_URL: server.url, BUILDKITE_ANALYTICS_KEY: 'valid-key' },
      }, (error, stdout, stderr) => error ? reject(error) : resolve({ stdout, stderr }))
    })
    expect(stdout).toContain(`SDK_ATTEMPTED=${enabled}`)
    expect(server.requests).toHaveLength(1)
    expect(server.requests[0].headers['content-type']).toBe('application/json')
    if (enabled) expect(stdout).toContain('could not be configured')
    else expect(stdout + stderr).not.toContain('[buildkite-test-collector]')
  })

  test.each(['', 'monorepo/'])('exports all attempts across specs with JSON-identical identity (prefix %s)', async (prefix) => {
    server = await startOtlpServer({ respond: (index) => index === 0 ? 503 : 200 })
    const env = {
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      BUILDKITE_ANALYTICS_BASE_URL: server.url,
      BUILDKITE_ANALYTICS_LOCATION_PREFIX: prefix,
      BUILDKITE_ANALYTICS_CYPRESS_TOKEN: 'config-token',
      BUILDKITE_TESTS_OTLP_TOKEN: ' relay-token ',
      OTEL_EXPORTER_OTLP_HEADERS: 'authorization=garbage,x-env-only=1',
      OTEL_EXPORTER_OTLP_TRACES_HEADERS: 'authorization=garbage2,x-env-traces=1',
      OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: 'http/json',
      OTEL_TRACES_SAMPLER: 'always_off',
      OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT: '2',
    }
    const otel = await run(env)
    expect(otel.code).toBe(3)
    expect(otel.stdout).not.toContain('Test Engine Sending:')
    expect(otel.stdout + otel.stderr).not.toContain('[buildkite-test-collector]')
    // A retried export can arrive after a later attempt's successful batch.
    const spans = server.spans().sort((a, b) => Number(a.startTimeUnixNano - b.startTimeUnixNano))
    // Eight tests, plus the first failed attempt of the retried test.
    expect(spans).toHaveLength(9)
    expect(new Set(spans.map((s) => s.attributes['buildkite.test.execution.external_id'])).size).toBe(9)
    const resultsByName = {}
    for (const span of spans) {
      const name = span.attributes['buildkite.test.name']
      resultsByName[name] ||= []
      resultsByName[name].push(span.attributes['test.case.result.status'])
    }
    expect(resultsByName).toEqual({
      'passes with a frozen browser clock': ['pass'],
      fails: ['fail'],
      'is skipped': ['skipped'],
      'passes on retry': ['fail', 'pass'],
      'cannot run': ['fail'],
      'is skipped after the hook failure': ['skipped'],
      'fails during teardown': ['fail'],
      'also exports': ['pass'],
    })
    for (const request of server.requests) {
      expect(request.decodeError).toBeNull()
      expect(request.headers).toMatchObject({
        authorization: 'Token token="relay-token"',
        'buildkite-tests-run-key': 'cypress-otel-e2e',
        'buildkite-tests-span-stream': 'test',
        'content-type': 'application/x-protobuf',
        'content-encoding': 'gzip',
      })
      expect(request.headers).not.toHaveProperty('x-env-only')
      expect(request.headers).not.toHaveProperty('x-env-traces')
    }
    expect(server.requests[0].status).toBe(503)
    for (const span of spans) {
      expect(span.name).toBe('test.execution')
      expect(span.parentSpanId).toBe('')
      expect(span.attributes).toMatchObject({
        'buildkite.execution.via': 'otlp',
        'buildkite.run_key': 'cypress-otel-e2e',
        'buildkite.test.framework.name': 'cypress',
        'buildkite.tag.hello': 'cypress',
        'buildkite.tag.ci.worker.id': 'cypress-agent',
      })
      expect(span.attributes['buildkite.test.name']).toBeTruthy()
      if (prefix) expect(span.attributes['code.file.path'].startsWith('monorepo/')).toBe(true)
      expect(span.attributes).not.toHaveProperty('code.line.number')
      expect(span.startTimeUnixNano > 0n).toBe(true)
      expect(span.endTimeUnixNano >= span.startTimeUnixNano).toBe(true)
      if (span.attributes['test.case.result.status'] === 'fail') {
        expect(span.status.code).toBe(2)
        expect(span.status.message).toBeTruthy()
        expect(span.events).toEqual([expect.objectContaining({
          name: 'exception',
          attributes: expect.objectContaining({ 'exception.stacktrace': expect.any(String) }),
        })])
      }
    }
    const frozen = spans.find((s) => s.attributes['buildkite.test.name'] === 'passes with a frozen browser clock')
    expect(frozen.startTimeUnixNano > BigInt(Date.UTC(2020, 0, 1)) * 1000000n).toBe(true)
    expect(frozen.endTimeUnixNano - frozen.startTimeUnixNano >= 50000000n).toBe(true)

    const json = await run({ ...env, BUILDKITE_ANALYTICS_TOKEN: 'json-token' }, '--reporter-options otelEnabled=false')
    expect(json.code).toBe(otel.code)
    const jsonRecords = records(json.stdout)
    expect(jsonRecords).toHaveLength(6) // Legacy JSON omits pending and hook-aborted tests.
    const spanRecords = spans.map((s) => ({
      scope: s.attributes['buildkite.test.scope'], name: s.attributes['buildkite.test.name'],
      file_name: s.attributes['code.file.path'], result: s.attributes['test.case.result.status'],
      failure_reason: s.status.message,
    }))
    for (const record of jsonRecords) {
      const final = spanRecords.filter((s) => identity(s) === identity(record)).pop()
      expect(final).toEqual({
        scope: record.scope, name: record.name, file_name: record.file_name,
        result: { passed: 'pass', failed: 'fail' }[record.result], failure_reason: record.failure_reason,
      })
      expect(record.location).toBe(record.file_name)
      if (prefix) expect(record.file_name.startsWith('monorepo/')).toBe(true)
    }
  }, 60000)

  test('uses the silent JSON path without a credential and preserves failed tests', async () => {
    server = await startOtlpServer()
    const result = await run({ BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url })
    expect(result.code).toBe(3)
    expect(result.stdout + result.stderr).not.toContain('[buildkite-test-collector]')
    expect(server.requests).toHaveLength(0)
  }, 30000)

  test('warns and falls back to JSON before tests for an invalid run key', async () => {
    server = await startOtlpServer()
    const result = await run({
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      BUILDKITE_ANALYTICS_BASE_URL: server.url,
      BUILDKITE_ANALYTICS_KEY: 'invalid key',
      BUILDKITE_ANALYTICS_CYPRESS_TOKEN: 'config-token',
    })
    expect(result.code).toBe(3)
    expect(result.stdout).toContain('uploading results as JSON instead')
    expect(records(result.stdout)).toHaveLength(6)
    expect(server.requests.every((r) => r.headers['content-type'] === 'application/json')).toBe(true)
  }, 30000)

  test('reports missing results without changing a passing exit code on export failure', async () => {
    server = await startOtlpServer()
    await server.close()
    const result = await run({
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url,
      BUILDKITE_ANALYTICS_CYPRESS_TOKEN: 'config-token',
    }, '--spec otel/second.spec.js')
    expect(result.code).toBe(0)
    expect(result.stdout).not.toContain('Test Engine Sending:')
    expect(result.stdout).toMatch(/TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span\(s\) \(export-failure, last OTLP failure: ECONNREFUSED\)/)
  }, 60000)
})
