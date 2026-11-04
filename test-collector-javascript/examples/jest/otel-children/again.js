// A second file in the same worker: built-in wrappers must record exactly
// once without being re-patched. Also covers bodies that run more than once or
// outside an ordinary async call, fresh module registries, and a child the
// test leaves open. beforeAll resets the module registry before the collector
// sets up, so setup loads a newer copy of the API than the one this file's
// tracer, the app's and the setup file's were obtained from.
const http = require('http')
const { trace } = require('@opentelemetry/api')
const app = require('./support/app')

const tracer = trace.getTracer('fixture-again')
let server
let url

beforeAll(async () => {
  jest.resetModules()
  server = app.createServer()
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  url = `http://127.0.0.1:${server.address().port}`
})
afterAll(() => new Promise((resolve) => server.close(resolve)))

const get = () => new Promise((resolve, reject) => {
  http.get(url, (res) => { res.resume(); res.on('end', resolve) }).on('error', reject)
})

test('http again', async () => {
  // A second file that overflows the child queue adds to the run's total.
  if (process.env.OTEL_CHILD_CASE === 'flood') for (let i = 0; i < 100; i++) tracer.startSpan('flood-child').end()
  await get()
})

describe('retried', () => {
  jest.retryTimes(1)
  let attempt = 0
  test('retried I/O', async () => {
    attempt += 1
    tracer.startSpan(`attempt-${attempt}`).end()
    await get()
    if (attempt === 1) throw new Error('first attempt fails')
  })
})

test('fake timers', () => {
  jest.useFakeTimers()
  try {
    setTimeout(() => tracer.startSpan('fake-timer-child').end(), 1000)
    jest.advanceTimersByTime(1000)
  } finally {
    jest.useRealTimers()
  }
})

test('generator body', function* () {
  yield Promise.resolve()
  tracer.startSpan('generator-child').end()
})

test.failing('failing body', async () => {
  tracer.startSpan('failing-child').end()
  throw new Error('expected to fail')
})

test('after jest.resetModules', () => {
  jest.resetModules()
  require('./support/app').work('reset-modules-child')
})

test('in jest.isolateModules', () => {
  jest.isolateModules(() => require('./support/app').work('isolated-modules-child'))
})

// Jest clears this sandbox timer at teardown, so the child never ends.
test('leaves a child open', () => {
  const span = tracer.startSpan('late-child')
  setTimeout(() => span.end(), 1000).unref()
})
