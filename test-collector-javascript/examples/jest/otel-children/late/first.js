const { trace, context, ROOT_CONTEXT } = require('@opentelemetry/api')

test('starts children for later', () => {
  const tracer = trace.getTracer('late-fixture')
  const span = tracer.startSpan('ends-in-next-file')
  // Built-ins are shared across sandboxes. The next file ends this span,
  // deterministically after this file's environment has torn down.
  require('timers')[Symbol.for('fixture.end-late-child')] = () => span.end()
  const parent = trace.setSpan(ROOT_CONTEXT, span)
  require('timers')[Symbol.for('fixture.start-late-child')] = (name, explicit) => {
    if (explicit) tracer.startSpan(name, {}, parent).end()
    else context.with(parent, () => tracer.startSpan(name).end())
  }
  tracer.startSpan('never-ends')
})
