// Child span export with the collector's provider. Files without a .test.js
// suffix keep these fixtures out of the plain example runs, and helpers live
// in otel-children/support.
const path = require('path')

const fixtures = path.join(__dirname, 'otel-children')
const mode = process.env.OTEL_CHILD_CASE || 'managed'

module.exports = {
  // OTEL_PROJECT_ROOT: a project that can load only some OpenTelemetry packages.
  rootDir: process.env.OTEL_PROJECT_ROOT || __dirname,
  roots: [fixtures],
  reporters: ['default', [require.resolve('buildkite-test-collector/jest/reporter'), { otelEnabled: true }]],
  testEnvironment: require.resolve('buildkite-test-collector/jest/environment'),
  ...(mode === 'no-runtime' ? {} : { runtime: require.resolve('buildkite-test-collector/jest/runtime') }),
  setupFilesAfterEnv: [path.join(fixtures, 'support/hooks.setup.js')],
  testLocationInResults: true,
  // OTEL_CHILD_CASE=optout runs a managed file, then one that opted out of the
  // collector's environment, in that order in one process.
  ...(mode === 'optout'
    ? { testMatch: [path.join(fixtures, 'io.js'), path.join(fixtures, 'optout/*.js')], testSequencer: path.join(fixtures, 'support/sequencer.js') }
    : { testMatch: [path.join(fixtures, '*.js')] }),
}
