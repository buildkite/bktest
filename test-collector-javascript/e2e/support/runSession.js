// Runs one export session scenario in a plain Node process and prints what the
// loopback receiver saw, so session.test.js can assert on real OTLP delivery
// outside Jest's sandbox.
//
// Usage: echo '<json scenario>' | node runSession.js; main() shows the
// scenario keys (stdin, because a long test name overflows one argv string).
// waitBeforeFlush can exercise a request started by the parent's 1 s timer.
const { startOtlpServer } = require('./otlpServer')
const { version } = require('../../package.json')
const fs = require('fs')
const os = require('os')
const path = require('path')

const scenario = JSON.parse(require('fs').readFileSync(0, 'utf8'))

const warnings = []
console.warn = (message) => warnings.push(message)

const responders = {
  '503-once': (index) => (index === 0 ? 503 : 200),
  // Retry-After: 0 makes the exporter retry at once, so its retries run out quickly.
  '503-always': () => ({ status: 503, headers: { 'Retry-After': '0' } }),
  401: () => 401,
  '401-slow': () => ({ status: 401, delay: 1500 }),
}

const main = async () => {
  const server = await startOtlpServer({ respond: responders[scenario.respond] })
  let endpoint = server.url
  if (scenario.unreachable) {
    const closed = await startOtlpServer()
    await closed.close()
    endpoint = closed.url
  }

  Object.assign(process.env, scenario.env || {})
  if (scenario.stubMathRandom) Math.random = () => 0
  const { createSession } = require('../../util/otel/session')
  const spool = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-session-'))
  const settings = {
    spool,
    endpoint,
    token: 'secret',
    runEnv: { ci: 'buildkite', key: 'run-1', collector: 'js-buildkite-test-collector', version, branch: 'main', commit_sha: 'abc' },
    tags: { hello: 'world' },
    testRunner: 'jest',
    frameworkVersion: '29.7.0',
  }
  const session = createSession(settings)
  const { createSender } = require('../../util/otel/exporter')
  const sender = createSender(settings, spool)

  for (const step of scenario.tests || []) {
    for (let i = 0; i < (step.repeat || 1); i++) {
      const handle = session.startTestSpan(step.start, { line: step.line, startTime: step.startTime })
      session.finishTestSpan(handle, 'finish' in step ? step.finish : step.start, { line: step.line, endTime: step.endTime })
    }
  }
  await session.forceFlush('session.test.js')
  sender.collect('session.test.js')
  if (scenario.waitBeforeFlush) await new Promise((resolve) => setTimeout(resolve, scenario.waitBeforeFlush))
  let flushed = null
  const counts = await sender.drain()
  const metrics = { exported: server.spans().length, failed: counts.test }
  if (scenario.forceFlush) {
    flushed = { requests: server.requests.map((request) => request.status), metrics, warnings: [...warnings] }
  }
  await session.shutdown()

  await new Promise((resolve) => setTimeout(resolve, 50))
  await server.close()
  fs.rmSync(spool, { recursive: true, force: true })

  process.stdout.write(JSON.stringify({
    requests: server.requests.map(({ status, headers, decodeError }) => ({ status, headers, decodeError: decodeError && decodeError.message })),
    resource: server.requests.find((request) => request.decoded)?.decoded.resourceSpans[0].resource,
    scope: server.requests.find((request) => request.decoded)?.decoded.resourceSpans[0].scopeSpans[0].scope,
    spans: server.spans(),
    warnings,
    metrics,
    flushed,
  }, (_key, value) => (typeof value === 'bigint' ? value.toString() : value)))
}

main().catch((error) => {
  process.stderr.write(`${error.stack}\n`)
  process.exit(1)
})
