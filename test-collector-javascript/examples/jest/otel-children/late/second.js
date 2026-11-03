const { trace } = require('@opentelemetry/api')

test('ends the previous file child', () => {
  const timers = require('timers')
  timers[Symbol.for('fixture.end-late-child')]()
  delete timers[Symbol.for('fixture.end-late-child')]
  trace.getTracer('late-fixture').startSpan('next-file-child').end()
})
