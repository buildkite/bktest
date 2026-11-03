const { trace } = require('@opentelemetry/api')

test('starts children for later', () => {
  const tracer = trace.getTracer('late-fixture')
  const span = tracer.startSpan('ends-in-next-file')
  // Built-ins are shared across sandboxes. The next file ends this span,
  // deterministically after this file's environment has torn down.
  require('timers')[Symbol.for('fixture.end-late-child')] = () => span.end()
  tracer.startSpan('never-ends')
})
