import { defineConfig } from 'vitest/config'
import { createRequire } from 'node:module'
const require = createRequire(import.meta.url)

export default defineConfig({ plugins: process.env.OTEL_TEST_GRAPHQL_CJS ? [{
  // The documented way to load GraphQL's CommonJS build, which its instrumentation patches.
  name: 'graphql-commonjs', enforce: 'pre', resolveId: (id) => (/^graphql(\/|$)/.test(id) ? require.resolve(id) : undefined),
}] : [], test: {
  include: process.env.OTEL_TEST_CASE === 'collisions' ? ['otel/collisions/*.test.js']
    // Not *.test.*, so the plain examples config does not run them.
    : process.env.OTEL_TEST_CASE === 'packages' ? ['otel/packages/cjs.js', 'otel/packages/esm.js', 'otel/packages/ts.ts']
    : process.env.OTEL_TEST_CASE ? [`otel/${process.env.OTEL_TEST_CASE}.js`] : ['otel/core.js', 'otel/second.js'],
  projects: process.env.OTEL_TEST_PROJECT_ROOT ? ['Aa', 'BB'].map(name => ({ test: {
    name, root: process.env.OTEL_TEST_PROJECT_ROOT, include: ['*.test.js'], setupFiles: [], env: { PROJECT_LABEL: name },
    pool: process.env.OTEL_TEST_POOL || 'forks', minWorkers: 1, maxWorkers: 1,
    experimental: process.env.OTEL_TEST_DISABLE_PROJECT === name ? { openTelemetry: { enabled: false } } : undefined,
  } })) : undefined,
  reporters: ['default', ['../../vitest/reporter.js', { otelEnabled: true, tags: { mode: 'vitest-otel' } }]],
  setupFiles: process.env.OTEL_TEST_PROJECT_ROOT ? [] : ['./otel/setup.js'],
  pool: process.env.OTEL_TEST_POOL || 'forks', minWorkers: 1, maxWorkers: 1,
  isolate: process.env.OTEL_TEST_ISOLATE !== 'false',
  environment: process.env.OTEL_TEST_ENVIRONMENT || (process.env.OTEL_TEST_CASE === 'tail' ? './otel/teardown-environment.js' : 'node'),
  includeTaskLocation: true,
  experimental: process.env.OTEL_TEST_NO_SDK ? {} : { openTelemetry: {
    enabled: true, sdkPath: process.env.OTEL_TEST_SDK || require.resolve('../../vitest/otel.js'),
  } },
} })
