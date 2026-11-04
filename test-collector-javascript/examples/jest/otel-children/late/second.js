const { trace } = require('@opentelemetry/api')

beforeAll(() => require('timers')[Symbol.for('fixture.start-late-child')]('before-next-file-activation', true))

test('ends the previous file child', () => {
  const timers = require('timers')
  timers[Symbol.for('fixture.start-late-child')]('started-in-next-file')
  delete timers[Symbol.for('fixture.start-late-child')]
  timers[Symbol.for('fixture.end-late-child')]()
  delete timers[Symbol.for('fixture.end-late-child')]
  trace.getTracer('late-fixture').startSpan('next-file-child').end()
})
