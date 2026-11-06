import { defineConfig } from 'vitest/config'
import { createRequire } from 'node:module'
const require = createRequire(import.meta.url)

export default defineConfig({ test: {
  include: process.env.OTEL_TEST_CASE === 'collisions' ? ['otel/collisions/*.test.js']
    : process.env.OTEL_TEST_CASE ? [`otel/${process.env.OTEL_TEST_CASE}.js`] : ['otel/core.js', 'otel/second.js'],
  projects: process.env.OTEL_TEST_PROJECT_ROOT ? ['Aa', 'BB'].map(name => ({ extends: true, test: {
    name, root: process.env.OTEL_TEST_PROJECT_ROOT, include: ['*.test.js'], setupFiles: [], env: { PROJECT_LABEL: name },
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
