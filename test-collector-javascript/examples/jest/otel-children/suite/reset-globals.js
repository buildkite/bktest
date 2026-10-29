// Suites that reset OpenTelemetry after each test, as its own tests do. The
// collector fills in what was reset at the next test, so later tests keep
// their children: from a tracer obtained at load, one obtained after the
// reset, an app module's, and http instrumentation.
const http = require('http')
const { trace, context } = require('@opentelemetry/api')
const app = require('../support/app')

const tracer = trace.getTracer('reset-globals')
let server
let url

beforeAll(async () => {
  server = app.createServer()
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  url = `http://127.0.0.1:${server.address().port}`
})
afterAll(() => new Promise((resolve) => server.close(resolve)))
afterEach(() => {
  trace.disable()
  context.disable()
})

const request = (name) => tracer.startActiveSpan(name, async (span) => {
  await new Promise((resolve, reject) => {
    http.get(url, (res) => { res.resume(); res.on('end', resolve) }).on('error', reject)
  })
  span.end()
})

test('reset first', () => request('reset-first'))

test('reset second', async () => {
  await request('reset-second')
  app.work('reset-second-app')
  trace.getTracer('after-reset').startSpan('reset-second-manual').end()
})
