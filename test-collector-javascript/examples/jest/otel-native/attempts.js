const { trace } = require('@opentelemetry/api')
const tracer = trace.getTracer('native-environment')
const child = (name) => tracer.startActiveSpan(name, (span) => span.end())

test('ordinary', () => {
  expect(typeof document).toBe(['jsdom', 'happy-dom'].includes(nativeEnvironment) ? 'object' : 'undefined')
  if (nativeEnvironment === 'custom') expect(nativeSetup).toBe(true)
  child('ordinary child')
})

test.concurrent.each([['slow', 35], ['fast', 5]])('%s', async (name, delay) => {
  await new Promise((resolve) => setTimeout(resolve, delay))
  child(`${name} child`)
})

describe('retry', () => {
  jest.retryTimes(1)
  let attempt = 0
  test('retried', () => {
    child(`retry child ${++attempt}`)
    expect(attempt).toBe(2)
  })
})

describe('blocked', () => {
  beforeAll(() => { throw new Error('native beforeAll failure') })
  test.each(['one', 'two'])('%s', () => { throw new Error('blocked body ran') })
})
