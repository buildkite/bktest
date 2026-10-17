const Mocha = require('mocha')
const Reporter = require('./reporter')
const { sessionFor } = require('../util/otel/session')
const { setWarningHandler } = require('../util/otel/warn')

jest.mock('../util/otel/session', () => ({ sessionFor: jest.fn() }))

test('maps interrupted Cypress results and warns instead of exporting unknown outcomes', async () => {
  const originalEnv = process.env
  process.env = { ...originalEnv, BUILDKITE_TESTS_OTLP_TOKEN: 'test', BUILDKITE_ANALYTICS_KEY: 'states' }
  const output = jest.spyOn(console, 'log').mockImplementation()
  const records = []
  const session = {
    startTestSpan: (record) => record,
    finishTestSpan: (_handle, record) => records.push(record),
    forceFlush: async () => {},
    shutdown: async () => {},
  }
  sessionFor.mockReturnValue(session)
  const beforeExit = process.listeners('beforeExit')
  try {
    const suite = new Mocha.Suite('states')
    const runner = new Mocha.Runner(suite)
    const reporter = new Reporter(runner, { reporterOptions: { otelEnabled: true } })
    const states = ['passed', 'failed', 'pending', 'skipped', 'processing', 'active', null, undefined, 'unexpected']
    states.forEach((state, index) => {
      const test = new Mocha.Test(`state ${index}`, () => {})
      test.file = 'states.cy.js'
      test.state = state
      suite.addTest(test)
      // The undefined state also exercises recovery at spec completion.
      if (state !== undefined) runner.emit('test:after:run', test)
    })
    const exits = []
    await reporter.done(2, (failures) => exits.push(failures))
    expect(exits).toEqual([2])
    expect(records.map(({ name, result }) => [name, result])).toEqual([
      ['state 0', 'passed'], ['state 1', 'failed'], ['state 2', 'pending'],
      ['state 3', 'skipped'], ['state 4', 'skipped'], ['state 5', 'skipped'],
      ['state 6', 'skipped'], ['state 7', 'skipped'],
    ])
    expect(output.mock.calls.map(([message]) => message)).toEqual([
      '[buildkite-test-collector] TEST RESULTS MISSING: Unknown Cypress test state "unexpected"; test execution was not exported.',
    ])

    session.forceFlush = async () => { throw new Error('unexpected flush failure') }
    await reporter.done(2, (failures) => exits.push(failures))
    expect(exits).toEqual([2, 2])
    expect(output.mock.calls.at(-1)[0]).toContain('TEST RESULTS MISSING: Could not finish Cypress OpenTelemetry reporting: Error: unexpected flush failure')
  } finally {
    process.env = originalEnv
    output.mockRestore()
    setWarningHandler((message) => console.warn(message))
    for (const listener of process.listeners('beforeExit')) {
      if (!beforeExit.includes(listener)) process.off('beforeExit', listener)
    }
  }
})
