const { execFile } = require('child_process')
const path = require('path')
const { startOtlpServer } = require('./support/otlpServer')
const version = require('vitest/package.json').version
const major = Number(version.split('.')[0])
const cwd = path.join(__dirname, '../examples/vitest')
const bin = path.join(path.dirname(require.resolve('vitest/package.json')), 'vitest.mjs')
let server
beforeEach(async () => { server = await startOtlpServer() })
afterEach(async () => { await server.close() })
const run = (env = {}, args = []) => new Promise(resolve => {
  execFile(process.execPath, [bin, 'run', '--config', 'otel.config.js', ...args], {
    cwd, env: { ...process.env,
      BUILDKITE_ANALYTICS_TOKEN: 'test-token', BUILDKITE_ANALYTICS_KEY: 'vitest-otel-e2e',
      BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url, ...env,
    },
  }, (error, stdout, stderr) => resolve({ code: error?.code || 0, stdout, stderr }))
})
const supported = major >= 4

;(supported ? test : test.skip).each(['forks', 'threads'])('native children and reporter roots agree (%s)', async pool => {
  const result = await run({ OTEL_TEST_POOL: pool })
  expect(result.code).toBe(1) // The fixture deliberately fails a hook.
  expect(result.stderr).not.toContain('TEST RESULTS MISSING')
  const spans = server.spans()
  const roots = spans.filter(s => s.name === 'test.execution')
  expect(roots).toHaveLength(8)
  expect(roots.every(s => !s.parentSpanId && s.attributes['buildkite.execution.via'] === 'otlp')).toBe(true)
  const root = roots.find(s => s.attributes['buildkite.test.name'] === 'body and IO')
  const children = spans.filter(s => s.traceId === root.traceId && s !== root)
  const byId = new Map(spans.map(s => [s.spanId, s]))
  for (const child of children) expect(byId.has(child.parentSpanId)).toBe(true)
  expect(byId.get(children.find(s => s.name === 'manual').parentSpanId).name).toBe('vitest.test.runner.test.callback')
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

test('older Vitest degrades explicitly without JSON fallback', async () => {
  if (major >= 4) return
  const result = await run({ OTEL_TEST_NO_SDK: 'true' })
  expect(result.stderr).toContain(major < 3 ? 'needs Vitest 3 or newer' : 'test.execution spans only')
  expect(server.spans()).toHaveLength(major < 3 ? 0 : 8)
}, 30000)
