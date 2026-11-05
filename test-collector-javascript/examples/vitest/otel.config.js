import { defineConfig } from 'vitest/config'
import { createRequire } from 'node:module'
const require = createRequire(import.meta.url)

export default defineConfig({ test: {
  include: ['otel/core.js', 'otel/second.js'],
  reporters: [['../../vitest/reporter.js', { otelEnabled: true, tags: { mode: 'vitest-otel' } }]],
  setupFiles: ['./otel/setup.js'],
  pool: process.env.OTEL_TEST_POOL || 'forks', maxWorkers: 1,
  isolate: process.env.OTEL_TEST_ISOLATE !== 'false',
  environment: process.env.OTEL_TEST_ENVIRONMENT || 'node',
  includeTaskLocation: true,
  experimental: process.env.OTEL_TEST_NO_SDK ? {} : { openTelemetry: {
    enabled: true, sdkPath: process.env.OTEL_TEST_SDK || require.resolve('../../vitest/otel.js'),
  } },
} })
