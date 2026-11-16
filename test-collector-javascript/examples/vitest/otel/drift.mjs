// Simulate changes to Vitest's experimental test spans.
import path from 'node:path'
import { createRequire } from 'node:module'
import { trace } from '@opentelemetry/api'
const require = createRequire(import.meta.url)

// Vitest may use either API build.
for (const apiTrace of new Set([trace, require('@opentelemetry/api').trace])) {
  const getTracer = apiTrace.getTracer.bind(apiTrace)
  apiTrace.getTracer = (name, ...rest) => {
    if (name !== 'vitest') return getTracer(name, ...rest)
    // This preload runs before SDK registration; don't cache a no-op tracer.
    const tracer = () => getTracer(process.env.OTEL_TEST_DRIFT === 'tracer' ? 'vitest-renamed' : name, ...rest)
    return {
      startSpan(...args) { return tracer().startSpan(...args) },
      startActiveSpan(spanName, options, ...args) {
        const file = options?.attributes?.['code.file.path']
        if (file && process.env.OTEL_TEST_DRIFT === 'path') options = { ...options, attributes: { ...options.attributes, 'code.file.path': path.relative(process.cwd(), file) } }
        return tracer().startActiveSpan(spanName, options, ...args)
      },
    }
  }
}
