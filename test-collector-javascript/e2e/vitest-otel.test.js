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
const drifted = 'experimental OpenTelemetry spans may have changed'

;(supported ? test : test.skip).each(['forks', 'threads'].flatMap(pool => [true, false].map(isolate => [pool, isolate])))('colliding Vitest IDs keep separate executions and children (%s, isolate=%s)', async (pool, isolate) => {
  const result = await run({ OTEL_TEST_CASE: 'collisions', OTEL_TEST_POOL: pool, OTEL_TEST_ISOLATE: String(isolate) })
  expect(result.code).toBe(0)
  const spans = server.spans()
  const roots = spans.filter(s => s.name === 'test.execution')
  expect(roots).toHaveLength(2)
  expect(roots[0].attributes['vitest.test.id']).toBe(roots[1].attributes['vitest.test.id'])
  expect(new Set(roots.map(s => s.traceId)).size).toBe(2)
  for (const [name, file, child] of [['first collision', 'Aa.test.js', 'first child'], ['second collision', 'BB.test.js', 'second child']]) {
    const root = roots.find(s => s.attributes['buildkite.test.name'] === name)
    expect(root.attributes['code.file.path']).toContain(file)
    expect(root.attributes['test.case.result.status']).toBe('pass')
    const trace = spans.filter(s => s.traceId === root.traceId)
    expect(trace.filter(s => ['first child', 'second child'].includes(s.name)).map(s => s.name)).toEqual([child])
    for (const span of trace.filter(s => s !== root)) expect(trace.some(parent => parent.spanId === span.parentSpanId)).toBe(true)
  }
  expect(result.stderr).not.toContain('dropped')
  expect(result.stderr).not.toContain(drifted)
  expect(result.stderr.match(/upstream ID hash collision/g)).toHaveLength(1)
  expect(result.stderr).toContain('Aa.test.js')
  expect(result.stderr).toContain('BB.test.js')
  expect(result.stderr).toContain('Renaming either file fixes it')
}, 30000)

;(supported ? test : test.skip).each(['forks', 'threads'])('accepted children completing during teardown are uploaded (%s)', async pool => {
  const result = await run({ OTEL_TEST_CASE: 'tail', OTEL_TEST_POOL: pool })
  expect(result.code).toBe(0)
  expect(result.stdout).toContain('CHILD ENDED')
  const spans = server.spans()
  const root = spans.find(s => s.name === 'test.execution')
  expect(root.attributes['test.case.result.status']).toBe('pass')
  const child = spans.find(s => s.name === 'accepted-child')
  expect(child).toBeDefined()
  expect(child.traceId).toBe(root.traceId)
  expect(spans.find(s => s.spanId === child.parentSpanId).name).toBe('vitest.test.runner.test.callback')
  expect(result.stderr).not.toMatch(/ENOENT|dropped|TEST RESULTS MISSING/)
}, 30000)

;(supported ? test : test.skip).each(['forks', 'threads'])('projects namespace IDs and discover project-only HTTP instrumentation (%s)', async pool => {
  // Outside the workspace: only this project's manifest declares instrumentation.
  fs.symlinkSync(path.join(__dirname, '../node_modules'), path.join(scratch, 'node_modules'), 'dir')
  fs.writeFileSync(path.join(scratch, 'package.json'), JSON.stringify({ type: 'module', devDependencies: {
    '@opentelemetry/instrumentation-http': '*', '@opentelemetry/instrumentation-undici': '*',
  } }))
  fs.writeFileSync(path.join(scratch, 'project.test.js'), `
    import { test } from 'vitest'
    import { trace } from '@opentelemetry/api'
    import { createServer, get } from 'node:http'
    test('project IO', async () => {
      trace.getTracer('app').startSpan('project-' + process.env.PROJECT_LABEL).end()
      const server = createServer((req, res) => res.end('ok'))
      await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
      const url = 'http://127.0.0.1:' + server.address().port
      try {
        await new Promise((resolve, reject) => get(url, res => { res.resume(); res.on('end', resolve) }).on('error', reject))
        await (await fetch(url)).text()
      } finally { await new Promise(resolve => server.close(resolve)) }
    })
  `)
  const result = await run({ OTEL_TEST_PROJECT_ROOT: scratch, OTEL_TEST_POOL: pool })
  expect(result).toEqual(expect.objectContaining({ code: 0 }))
  const spans = server.spans()
  const roots = spans.filter(s => s.name === 'test.execution')
  expect(roots).toHaveLength(2)
  expect(roots[0].attributes['vitest.test.id']).toBe(roots[1].attributes['vitest.test.id'])
  expect(new Set(roots.map(s => s.traceId)).size).toBe(2)
  for (const name of ['project-Aa', 'project-BB']) {
    const child = spans.find(s => s.name === name)
    expect(child).toBeDefined()
    expect(roots.some(root => root.traceId === child.traceId)).toBe(true)
    const children = spans.filter(s => s.traceId === child.traceId)
    expect(children.filter(s => s.kind === 3)).toHaveLength(2)
    expect(children.filter(s => s.kind === 2)).toHaveLength(2)
  }
  expect(result.stderr).not.toMatch(/disabled|dropped|TEST RESULTS MISSING/)
  expect(result.stderr).not.toContain(drifted)
  server.requests.length = 0
  const partial = await run({ OTEL_TEST_PROJECT_ROOT: scratch, OTEL_TEST_POOL: pool, OTEL_TEST_DISABLE_PROJECT: 'BB' })
  expect(partial.code).toBe(0)
  expect(server.spans().filter(s => s.name === 'test.execution')).toHaveLength(2)
  expect(server.spans().some(s => s.name === 'project-Aa')).toBe(true)
  expect(server.spans().some(s => s.name === 'project-BB')).toBe(false)
  expect(partial.stderr).toContain('children are disabled for projects: BB.')
  expect(partial.stderr).not.toContain('children are disabled for projects: Aa')
  expect(partial.stderr).not.toContain(drifted)
}, 30000)

;(supported ? test : test.skip).each(['forks', 'threads'].flatMap(pool =>
  ['node', 'jsdom', 'happy-dom'].flatMap(environment => [true, false].map(isolate => [pool, environment, isolate]))
))('native children and reporter roots agree (%s, %s, isolate=%s)', async (pool, environment, isolate) => {
  const result = await run({ OTEL_TEST_POOL: pool, OTEL_TEST_ENVIRONMENT: environment, OTEL_TEST_ISOLATE: String(isolate) })
  expect(result.code).toBe(1) // The fixture deliberately fails a hook.
  expect(result.stdout).toContain('BEFORE ALL TRACEPARENTS [null,null,null]')
  expect(result.stderr).not.toContain('TEST RESULTS MISSING')
  expect(result.stderr).not.toContain(drifted)
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
  expect(spans.some(s => s.traceId === failure.traceId && ['before-hook', 'setup-hook'].includes(s.name))).toBe(false)
  const annotated = roots.find(s => s.attributes['buildkite.test.name'] === 'concurrent A')
  const phase = spans.find(s => s.traceId === annotated.traceId && s.name === 'vitest.test.runner.test.afterEach')
  expect(phase.events.some(e => e.name === 'cleanup-note')).toBe(true)
  expect(spans.some(s => s.parentSpanId === phase.spanId)).toBe(false)
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
  const result = await run()
  expect(result.stderr).toContain(major < 3 ? 'needs Vitest 3 or newer' : 'test.execution spans only')
  expect(server.spans()).toHaveLength(major < 3 ? 0 : 8)
  if (major < 3) expect(server.requests).toHaveLength(0)
}, 30000)

;(supported ? test : test.skip).each(['forks', 'threads'])('bail and a crashed worker still have reporter-owned executions (%s)', async pool => {
  const bail = await run({ OTEL_TEST_CASE: 'bail', OTEL_TEST_POOL: pool }, ['--bail', '1'])
  expect(bail.code).toBe(1)
  expect(server.spans().filter(s => s.name === 'test.execution').map(s => s.attributes['test.case.result.status'])).toEqual(['fail', 'skipped', 'skipped'])
  server.requests.length = 0
  const crash = await run({ OTEL_TEST_CASE: 'crash', OTEL_TEST_POOL: pool })
  expect(crash.code).toBe(1)
  const roots = server.spans().filter(s => s.name === 'test.execution')
  expect(roots.map(s => [s.attributes['buildkite.test.name'], s.attributes['test.case.result.status']])).toEqual([
    ['passes before worker crashes', 'pass'], ['worker crashes', 'skipped'], ['never reached', 'skipped'],
  ])
  expect(roots.every(s => !s.parentSpanId)).toBe(true)
  expect(crash.stderr).not.toContain('TEST RESULTS MISSING')
  for (const { stderr } of [bail, crash]) expect(stderr).not.toContain(drifted)
}, 30000)

;(supported ? test : test.skip).each(['forks', 'threads'])('changes to Vitest test spans warn instead of silently losing children (%s)', async pool => {
  const preload = { NODE_OPTIONS: [process.env.NODE_OPTIONS, '--require', path.join(cwd, 'otel/drift.cjs')].filter(Boolean).join(' ') }
  const renamed = await run({ ...preload, OTEL_TEST_DRIFT: 'tracer', OTEL_TEST_POOL: pool })
  expect(renamed.stderr).toContain(`children were not recorded because no test span was recognised. Vitest ${version}'s ${drifted}`)
  expect(server.spans()).toHaveLength(8)
  expect(server.spans().every(s => s.name === 'test.execution')).toBe(true)
  server.requests.length = 0
  const relative = await run({ ...preload, OTEL_TEST_DRIFT: 'path', OTEL_TEST_POOL: pool })
  expect(relative.stderr).toContain(`children could not be linked to their tests. Vitest ${version}'s ${drifted}`)
  const spans = server.spans()
  const roots = spans.filter(s => s.name === 'test.execution')
  expect(roots).toHaveLength(8)
  expect(spans.length).toBeGreaterThan(8)
  expect(spans.filter(s => s.name !== 'test.execution').some(s => roots.some(root => root.traceId === s.traceId))).toBe(false)
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
  server.requests.length = 0
  await run({ OTEL_SPAN_EVENT_COUNT_LIMIT: '0', OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT: '4' })
  const child = server.spans().find(s => s.name === 'db.connect')
  expect(child.events).toHaveLength(0)
  expect(child.attributes['db.system.name']).toBe('post')
  const roots = server.spans().filter(s => s.name === 'test.execution')
  expect(roots).toHaveLength(8)
  const failure = roots.find(s => s.attributes['buildkite.test.name'] === 'hook failure')
  expect(failure.events.some(e => e.name === 'exception')).toBe(true)
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
  ['http', '', 1, 'forks', 'always_off'],
  ['http,undici', 'http', 1, 'threads', 'always_off'],
  ['http,undici', 'http,undici', 0, 'forks', 'traceidratio'],
])('instrumentation selection enabled=%s disabled=%s clients=%s (%s, sampler=%s)', async (enabled, disabled, clients, pool, sampler) => {
  await run({ OTEL_NODE_ENABLED_INSTRUMENTATIONS: enabled, OTEL_NODE_DISABLED_INSTRUMENTATIONS: disabled })
  expect(server.spans().filter(s => s.kind === 3)).toHaveLength(clients)
  expect(server.spans().filter(s => s.name === 'test.execution')).toHaveLength(8)
  server.requests.length = 0
  const sampled = await run({ OTEL_TEST_POOL: pool, OTEL_TRACES_SAMPLER: sampler, OTEL_TRACES_SAMPLER_ARG: '0' })
  expect(sampled.stderr).not.toContain(drifted)
  expect(server.spans()).toHaveLength(8)
  expect(server.spans().every(s => s.name === 'test.execution')).toBe(true)
}, 30000)
