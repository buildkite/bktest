// Preloaded to simulate Vitest releases that change the experimental test
// spans the collector reads: a renamed tracer, or relative file paths.
const path = require('path')
const { trace } = require('@opentelemetry/api')

const getTracer = trace.getTracer.bind(trace)
trace.getTracer = (name, ...rest) => {
  if (name !== 'vitest') return getTracer(name, ...rest)
  if (process.env.OTEL_TEST_DRIFT === 'tracer') return getTracer('vitest-renamed', ...rest)
  const tracer = getTracer(name, ...rest)
  return Object.assign(Object.create(tracer), {
    startActiveSpan(spanName, options, ...args) {
      const file = options?.attributes?.['code.file.path']
      if (file) options = { ...options, attributes: { ...options.attributes, 'code.file.path': path.relative(process.cwd(), file) } }
      return tracer.startActiveSpan(spanName, options, ...args)
    },
  })
}
