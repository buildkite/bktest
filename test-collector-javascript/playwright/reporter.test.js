const Reporter = require('./reporter')

describe('testEngineResult', () => {
  const reporter = new Reporter({})

  test.each([
    ['failed', undefined, 'failed'],
    ['passed', undefined, 'passed'],
    ['skipped', undefined, 'skipped'],
    ['timedOut', undefined, 'failed'],
    ['interrupted', undefined, 'unknown'],
    ['failed', 'failed', 'passed'],
    ['passed', 'failed', 'passed'],
    ['timedOut', 'failed', 'failed'],
    ['skipped', 'failed', 'skipped'],
    ['skipped', 'skipped', 'skipped'],
    ['passed', 'skipped', 'unknown'],
    ['failed', 'skipped', 'unknown'],
    ['timedOut', 'skipped', 'unknown'],
    ['interrupted', 'skipped', 'unknown'],
    ['passed', 'passed', 'passed'],
    ['failed', 'passed', 'failed'],
  ])('maps status %s with expected status %s to %s', (status, expectedStatus, expected) => {
    expect(reporter.testEngineResult(status, expectedStatus)).toBe(expected)
  })
})

describe('testEngineRecord', () => {
  const reporter = new Reporter({})
  const testCase = (expectedStatus) => ({
    id: 'test-id',
    title: 'example',
    titlePath: () => ['example'],
    location: { file: 'tests/example.spec.js', line: 1, column: 1 },
    expectedStatus,
    tags: [],
  })
  const result = (status) => ({ status, startTime: new Date(0), duration: 1 })

  test('records an expected failure as passed', () => {
    expect(reporter.testEngineRecord(testCase('failed'), result('failed')).result).toBe('passed')
  })

  test('records a test expected to be skipped but not skipped as unknown', () => {
    expect(reporter.testEngineRecord(testCase('skipped'), result('passed')).result).toBe('unknown')
  })
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
