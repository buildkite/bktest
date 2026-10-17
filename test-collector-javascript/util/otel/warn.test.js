const { warn, setWarningHandler } = require('./warn')

test('routes the same prefixed warning to a runner-specific handler', () => {
  const output = jest.fn()
  try {
    setWarningHandler(output)
    warn('TEST RESULTS MISSING: example')
    expect(output).toHaveBeenCalledWith('[buildkite-test-collector] TEST RESULTS MISSING: example')
  } finally {
    setWarningHandler((message) => console.warn(message))
  }
})
