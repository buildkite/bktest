/**
 * @jest-environment node
 */
test('runs under the node environment', () => {
  expect(typeof setImmediate).toBe('function')
  require('@opentelemetry/api').trace.getTracer('docblock').startSpan('docblock child').end()
})
