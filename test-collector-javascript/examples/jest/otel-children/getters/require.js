const { trace } = require('@opentelemetry/api')
const dependency = require('../support/getter-exports')

test('requires getter-only exports', () => {
  expect(dependency).toEqual({ answer: 7 })
  trace.getTracer('getter-fixture').startSpan('getter work').end()
})
