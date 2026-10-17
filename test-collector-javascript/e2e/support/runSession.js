// Runs one export session scenario in a plain Node process and prints what the
// loopback receiver saw, so session.test.js can assert on it. Jest runs test
// files inside a vm context where the OTLP exporter's dynamic import('http')
// is refused, so export behaviour is observed from outside instead.
//
// Usage: node runSession.js '<json scenario>'; main() shows the scenario
// keys. waitBeforeFlush should exceed the processor's 1 s schedule delay so
// its timer hands off a batch before forceFlush().
const { startOtlpServer } = require('./otlpServer')
const { version } = require('../../package.json')

const scenario = JSON.parse(process.argv[2])

const warnings = []
console.warn = (message) => warnings.push(message)

const responders = {
  '503-once': (index) => (index === 0 ? 503 : 200),
  401: () => 401,
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
  const session = createSession({
    endpoint,
    token: 'secret',
    runEnv: { ci: 'buildkite', key: 'run-1', collector: 'js-buildkite-test-collector', version, branch: 'main', commit_sha: 'abc' },
    tags: { hello: 'world' },
    testRunner: 'jest',
    frameworkVersion: '29.7.0',
  })

  for (const step of scenario.tests || []) {
    const handle = session.startTestSpan(step.start, { line: step.line, column: step.column, startTime: step.startTime })
    session.finishTestSpan(handle, 'finish' in step ? step.finish : step.start, { line: step.line, column: step.column, endTime: step.endTime })
  }
  if (scenario.waitBeforeFlush) await new Promise((resolve) => setTimeout(resolve, scenario.waitBeforeFlush))
  let flushed = null
  if (scenario.forceFlush) {
    await session.forceFlush()
    flushed = { requests: server.requests.map((request) => request.status), metrics: { ...session.metrics }, warnings: [...warnings] }
  }
  await session.shutdown()

  await new Promise((resolve) => setTimeout(resolve, 50))
  await server.close()

  process.stdout.write(JSON.stringify({
    requests: server.requests.map(({ status, headers, decodeError }) => ({ status, headers, decodeError: decodeError && decodeError.message })),
    resource: server.requests.find((request) => request.decoded)?.decoded.resourceSpans[0].resource,
    scope: server.requests.find((request) => request.decoded)?.decoded.resourceSpans[0].scopeSpans[0].scope,
    spans: server.spans(),
    warnings,
    metrics: session.metrics,
    flushed,
  }, (_key, value) => (typeof value === 'bigint' ? value.toString() : value)))
}

main().catch((error) => {
  process.stderr.write(`${error.stack}\n`)
  process.exit(1)
})
