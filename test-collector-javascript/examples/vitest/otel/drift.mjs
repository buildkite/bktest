// Preloaded to simulate Vitest releases that change the experimental test
// spans the collector reads: a renamed tracer, or relative file paths.
import path from 'node:path'
import { createRequire } from 'node:module'
import { trace } from '@opentelemetry/api'
const require = createRequire(import.meta.url)

// API 1.10's CJS and ESM builds have separate trace objects. Vitest may use
// either build across versions and pools, so perturb both.
for (const apiTrace of new Set([trace, require('@opentelemetry/api').trace])) {
  const getTracer = apiTrace.getTracer.bind(apiTrace)
  apiTrace.getTracer = (name, ...rest) => {
    if (name !== 'vitest') return getTracer(name, ...rest)
    // Importing ESM in this preload can beat SDK registration. Resolve the
    // current provider at use time, rather than retaining its empty ESM proxy.
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
