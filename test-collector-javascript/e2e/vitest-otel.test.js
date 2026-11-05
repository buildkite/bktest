const { execFile } = require('child_process')
const path = require('path')
const fs = require('fs')
const os = require('os')
const { startOtlpServer } = require('./support/otlpServer')
const version = require('vitest/package.json').version
const major = Number(version.split('.')[0])
const cwd = path.join(__dirname, '../examples/vitest')
const bin = path.join(path.dirname(require.resolve('vitest/package.json')), 'vitest.mjs')
let server, scratch
beforeEach(async () => {
  server = await startOtlpServer()
  scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'vitest-otel-e2e-'))
})
afterEach(async () => {
  await server.close()
  fs.rmSync(scratch, { recursive: true, force: true })
})
const run = (env = {}, args = []) => new Promise(resolve => {
  execFile(process.execPath, [bin, 'run', '--config', 'otel.config.js', ...args], {
    cwd, env: { ...process.env,
      BUILDKITE_ANALYTICS_TOKEN: 'test-token', BUILDKITE_ANALYTICS_KEY: 'vitest-otel-e2e',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url, BUILDKITE_ANALYTICS_BASE_URL: server.url, ...env,
    },
  }, (error, stdout, stderr) => resolve({ code: error?.code || 0, stdout, stderr }))
})
const supported = major >= 4

;(supported ? test : test.skip).each(['forks', 'threads'].flatMap(pool =>
  ['node', 'jsdom', 'happy-dom'].flatMap(environment => [true, false].map(isolate => [pool, environment, isolate]))
))('native children and reporter roots agree (%s, %s, isolate=%s)', async (pool, environment, isolate) => {
  const result = await run({ OTEL_TEST_POOL: pool, OTEL_TEST_ENVIRONMENT: environment, OTEL_TEST_ISOLATE: String(isolate) })
  expect(result.code).toBe(1) // The fixture deliberately fails a hook.
  expect(result.stderr).not.toContain('TEST RESULTS MISSING')
  const spans = server.spans()
  const roots = spans.filter(s => s.name === 'test.execution')
  expect(roots).toHaveLength(8)
  expect(roots.every(s => !s.parentSpanId && s.attributes['buildkite.execution.via'] === 'otlp')).toBe(true)
  const root = roots.find(s => s.attributes['buildkite.test.name'] === 'body and IO')
  expect(root.attributes['code.column.number']).toBeGreaterThan(0n)
  const children = spans.filter(s => s.traceId === root.traceId && s !== root)
  const byId = new Map(spans.map(s => [s.spanId, s]))
  for (const child of children) expect(byId.has(child.parentSpanId)).toBe(true)
  for (const name of ['manual', 'db.connect']) expect(byId.get(children.find(s => s.name === name).parentSpanId).name).toBe('vitest.test.runner.test.callback')
  for (const name of ['before-hook', 'setup-hook']) expect(byId.get(children.find(s => s.name === name).parentSpanId).name).toBe('vitest.test.runner.test.beforeEach')
  expect(byId.get(children.find(s => s.name === 'after-hook').parentSpanId).name).toBe('vitest.test.runner.test.afterEach')
  expect(children.filter(s => s.kind === 3)).toHaveLength(2)
  expect(children.filter(s => s.kind === 2)).toHaveLength(2)
  for (const handler of children.filter(s => s.name === 'handler')) {
    const incoming = byId.get(handler.parentSpanId)
    expect(incoming.kind).toBe(2)
    expect(byId.get(incoming.parentSpanId).kind).toBe(3)
  }
  expect(spans.some(s => s.name.includes('before-all-not-a-child'))).toBe(false)
  expect(spans.some(s => s.name === 'vitest.test.runner.test.cleanup')).toBe(false)
  const failure = roots.find(s => s.attributes['buildkite.test.name'] === 'hook failure')
  const hook = spans.find(s => s.traceId === failure.traceId && s.name === 'vitest.test.runner.test.beforeEach')
  expect(hook.status.code).toBe(2)
  expect(hook.events.some(e => e.name === 'exception')).toBe(true)
  const retry = roots.find(s => s.attributes['buildkite.test.name'] === 'retry')
  expect(spans.filter(s => s.traceId === retry.traceId && s.name === 'attempt')).toHaveLength(2)
  for (const name of ['concurrent A', 'concurrent B', 'second file']) {
    const execution = roots.find(s => s.attributes['buildkite.test.name'] === name)
    expect(spans.filter(s => s.traceId === execution.traceId).map(s => s.name)).toContain(name === 'second file' ? 'second-body' : name)
  }
  expect(roots.filter(s => s.attributes['test.case.result.status'] === 'skipped')).toHaveLength(2)
  expect(spans.filter(s => s.kind === 3)).toHaveLength(2) // No exporter POSTs.
}, 30000)

;(major < 4 ? test : test.skip)('older Vitest degrades explicitly without JSON fallback', async () => {
  const result = await run({ OTEL_TEST_NO_SDK: 'true' })
  expect(result.stderr).toContain(major < 3 ? 'needs Vitest 3 or newer' : 'test.execution spans only')
  expect(server.spans()).toHaveLength(major < 3 ? 0 : 8)
  if (major < 3) expect(server.requests).toHaveLength(0)
}, 30000)

;(supported ? test : test.skip)('bail and a crashed worker still have reporter-owned executions', async () => {
  const bail = await run({ OTEL_TEST_CASE: 'bail' }, ['--bail', '1'])
  expect(bail.code).toBe(1)
  expect(server.spans().filter(s => s.name === 'test.execution').map(s => s.attributes['test.case.result.status'])).toEqual(['fail', 'skipped', 'skipped'])
  server.requests.length = 0
  const crash = await run({ OTEL_TEST_CASE: 'crash' })
  expect(crash.code).toBe(1)
  expect(server.spans().filter(s => s.name === 'test.execution')).toHaveLength(2)
}, 30000)

;(supported ? test : test.skip)('late children and the child cap warn without losing test results', async () => {
  const late = await run({ OTEL_TEST_CASE: 'late' })
  expect(late.stderr).toContain('started after file teardown: 1')
  expect(server.spans().some(s => s.name === 'too-late')).toBe(false)
  expect(server.spans().some(s => s.name === 'on-time')).toBe(true)
  server.requests.length = 0
  const capped = await run({ OTEL_TEST_CASE: 'second', BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE: '2' })
  expect(capped.stderr).toContain('per-file cap 2')
  expect(server.spans().filter(s => s.name === 'test.execution')).toHaveLength(1)
  expect(server.spans().filter(s => s.name !== 'test.execution')).toHaveLength(2)
  for (const span of server.spans().filter(s => s.name !== 'test.execution')) {
    expect(server.spans().some(parent => parent.spanId === span.parentSpanId)).toBe(true)
  }
  server.requests.length = 0
  await run({ OTEL_TEST_CASE: 'second', BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE: '1' })
  // A one-span budget keeps the user's span, not an empty phase ancestor.
  expect(server.spans().map(s => s.name).sort()).toEqual(['setup-hook', 'test.execution'])
}, 30000)

;(supported ? test : test.skip)('user-owned SDK and absent collector reporter have clear warnings', async () => {
  const custom = await run({ OTEL_TEST_SDK: path.join(cwd, 'otel/user-sdk.cjs') })
  expect(custom.stderr).toContain('test.execution spans only')
  expect(server.spans()).toHaveLength(8)
  server.requests.length = 0
  const cli = await run({}, ['--reporter=default', '--reporter=json', `--outputFile=${path.join(scratch, 'results.json')}`])
  expect(cli.stderr).toContain('CLI --reporter flags replace config reporters')
  expect(server.spans()).toHaveLength(0)
  expect(JSON.parse(fs.readFileSync(path.join(scratch, 'results.json'))).numTotalTests).toBe(8)
}, 30000)

;(supported ? test : test.skip).each([
  ['http', '', 1], ['http,undici', 'http', 1], ['http,undici', 'http,undici', 0],
])('instrumentation selection enabled=%s disabled=%s', async (enabled, disabled, clients) => {
  await run({ OTEL_NODE_ENABLED_INSTRUMENTATIONS: enabled, OTEL_NODE_DISABLED_INSTRUMENTATIONS: disabled })
  expect(server.spans().filter(s => s.kind === 3)).toHaveLength(clients)
  expect(server.spans().filter(s => s.name === 'test.execution')).toHaveLength(8)
}, 30000)
