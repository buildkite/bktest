const { appTracingEnv } = require('buildkite-test-collector/playwright/fixture')

const origin = `http://127.0.0.1:${process.env.PLAYWRIGHT_APP_PORT}`

module.exports = {
  ...require('./otel.config'),
  testMatch: 'app.js',
  workers: 4,
  fullyParallel: true,
  retries: 1,
  projects: ['chromium', 'firefox', 'webkit'].map(browserName => ({ name: browserName, use: { browserName } })),
  use: { baseURL: origin },
  webServer: [{
    command: 'node --require ./otel/app/instrumentation.js ./otel/app/server.js',
    url: `${origin}/health`,
    env: { ...appTracingEnv(), NODE_ENV: 'test', OTEL_METRICS_EXPORTER: 'none', OTEL_LOGS_EXPORTER: 'none' },
    gracefulShutdown: { signal: 'SIGTERM', timeout: 5000 },
  }],
}
