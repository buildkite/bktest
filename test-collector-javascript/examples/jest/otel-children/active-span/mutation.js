const { trace } = require('@opentelemetry/api')

test('cannot change the test result through the active span', () => {
  const span = trace.getActiveSpan()
  expect(span).toBeDefined()
  span.updateName('x')
  span.setStatus({ code: 2 })
  span.setAttribute('user.id', 1)
  span.end()
  trace.getTracer('app').startSpan('manual child').end()
})
