// A second file in the same worker: built-in modules were patched for the
// previous file and unpatched at its teardown, and must be patched exactly
// once again. Also covers test shapes that run a body more than once or
// outside an ordinary async call.
const http = require('http')
const { trace } = require('@opentelemetry/api')

const tracer = trace.getTracer('fixture-again')
const major = Number(require('jest/package.json').version.split('.')[0])
let server
let url

beforeAll(async () => {
  server = http.createServer((req, res) => res.end('again'))
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  url = `http://127.0.0.1:${server.address().port}`
})
afterAll(() => new Promise((resolve) => server.close(resolve)))

const get = () => new Promise((resolve, reject) => {
  http.get(url, (res) => { res.resume(); res.on('end', resolve) }).on('error', reject)
})

test('http again', async () => {
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
  // Jest 28's modern fake timers cannot fake performance on Node 19+.
  jest.useFakeTimers(major === 27 ? 'modern' : { doNotFake: major === 28 ? ['performance'] : [] })
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

if (major >= 28) {
  test.failing('failing body', async () => {
    tracer.startSpan('failing-child').end()
    throw new Error('expected to fail')
  })
} else {
  test('failing body', () => { tracer.startSpan('failing-child').end() })
}
