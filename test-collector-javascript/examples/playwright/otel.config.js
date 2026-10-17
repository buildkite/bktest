module.exports = {
  testDir: './otel',
  testMatch: 'outcomes.js',
  workers: 2,
  reporter: [
    ['line'],
    ['json', { outputFile: process.env.PLAYWRIGHT_JSON_OUTPUT_NAME }],
    ['buildkite-test-collector/playwright/reporter', {
      otelEnabled: true,
      tags: { hello: 'playwright', priority: 'run', 'ci.worker.id': 'configured-worker' },
    }],
  ],
}
