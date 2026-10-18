const Reporter = require('./reporter')

test('counts a record-builder failure in the end-of-run dropped-span warning', async () => {
  const originalEnv = process.env
  process.env = { ...originalEnv, BUILDKITE_ANALYTICS_KEY: 'playwright-record-failure', BUILDKITE_ANALYTICS_OTLP_ENDPOINT: 'http://127.0.0.1:1/v1/traces' }
  const warning = jest.spyOn(console, 'warn').mockImplementation()
  const reporter = new Reporter({ otelEnabled: true, token: 'test-token' })
  reporter.onBegin({ version: '1.56.1' })
  jest.spyOn(reporter, 'testEngineRecord').mockImplementation(() => { throw new Error('record failed') })

  try {
    reporter.onTestEnd({}, {})
    expect(warning.mock.calls.map(([message]) => message)).toEqual([
      '[buildkite-test-collector] TEST RESULTS MISSING: Could not report the Playwright test execution over OpenTelemetry: Error: record failed',
    ])
    expect(reporter._session.metrics.startFailures).toBe(1)

    await reporter.onEnd()
    expect(warning.mock.calls.map(([message]) => message)).toEqual([
      expect.stringContaining('Could not report the Playwright test execution'),
      '[buildkite-test-collector] TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (start-failure).\n' +
      '[buildkite-test-collector] OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.',
    ])
  } finally {
    await reporter._session.shutdown()
    warning.mockRestore()
    process.env = originalEnv
  }
})
