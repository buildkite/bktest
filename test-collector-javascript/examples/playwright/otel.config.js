module.exports = {
  testDir: './otel',
  testMatch: 'outcomes.js',
  globalSetup: process.env.PLAYWRIGHT_OTEL_SETUP_TOKEN ? require.resolve('./otel-global-setup') : undefined,
  workers: 2,
  reporter: [
    ['line'],
    ['json', { outputFile: process.env.PLAYWRIGHT_JSON_OUTPUT_NAME }],
    [require.resolve('./otel-steps-observer')],
    ['buildkite-test-collector/playwright/reporter', {
      otelEnabled: process.env.PLAYWRIGHT_OTEL_ENABLED === undefined ? true : JSON.parse(process.env.PLAYWRIGHT_OTEL_ENABLED),
      tags: { hello: 'playwright', priority: 'run', 'ci.worker.id': 'configured-worker' },
    }],
  ],
}
