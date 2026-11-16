const Reporter = require('./reporter')

test('stops advertising app tracing when a rerun disables OTel', async () => {
  const originalEnv = process.env
  process.env = { ...originalEnv, BUILDKITE_ANALYTICS_KEY: 'playwright-rerun', BUILDKITE_TESTS_OTLP_TOKEN: 'test-token' }
  const reporter = new Reporter({ otelEnabled: true })
  try {
    reporter.onBegin({ version: '1.56.1' })
    expect(JSON.parse(process.env.BUILDKITE_TEST_COLLECTOR_PLAYWRIGHT_OTEL).seed).toEqual(expect.any(String))
    await reporter.onEnd()
    new Reporter({ otelEnabled: false }).onBegin({ version: '1.56.1' })
    expect(JSON.parse(process.env.BUILDKITE_TEST_COLLECTOR_PLAYWRIGHT_OTEL)).toBeNull()
  } finally {
    await reporter.onEnd()
    process.env = originalEnv
  }
})

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

    await reporter.onEnd()
    expect(warning.mock.calls.map(([message]) => message)).toEqual([
      expect.stringContaining('Could not report the Playwright test execution'),
      '[buildkite-test-collector] TEST RESULTS MISSING: 1 Playwright test execution(s) were not recorded; fix the earlier warnings and rerun (no JSON fallback).',
    ])
  } finally {
    await reporter._session.shutdown()
    warning.mockRestore()
    process.env = originalEnv
  }
})
