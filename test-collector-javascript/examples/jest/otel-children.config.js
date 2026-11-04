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
  runtime: require.resolve('buildkite-test-collector/jest/runtime'),
  // Its CommonJS require of the API would load the copy an ES module imports.
  setupFilesAfterEnv: ['esm', 'aliases', 'mocked-api', 'mapped-api', 'getters'].includes(mode) ? [] : [path.join(fixtures, 'support/hooks.setup.js')],
  testLocationInResults: true,
  ...(mode === 'mapped-api' ? { resetMocks: true, moduleNameMapper: { '^@opentelemetry/api$': path.join(fixtures, 'support/mapped-api.js') } } : {}),
  ...(mode === 'aliases' ? { moduleNameMapper: {
    '^@fixture/web$': 'express',
    '^express$': process.env.OTEL_MAPPED_MOCK ? path.join(fixtures, 'support/mapped-web.js') : 'express',
  } } : {}),
  // Suite files configure their own providers; native ESM is untransformed.
  ...(mode === 'interceptors'
    ? { testMatch: [path.join(fixtures, 'io.js'), path.join(fixtures, `${mode}/*.js`)], testSequencer: path.join(fixtures, 'support/sequencer.js') }
    : { testMatch: [path.join(fixtures, { suite: 'suite/*.js', esm: 'esm/*.mjs', aliases: 'aliases/*.js', late: 'late/*.js', pg: 'pg/*.js', 'mocked-api': 'mocked-api/*.js', 'mapped-api': 'mapped-api/*.js', 'active-span': 'active-span/*.js', getters: 'getters/*.js' }[mode] || '*.js')] }),
  ...(mode === 'late' ? { testSequencer: path.join(fixtures, 'support/sequencer.js') } : {}),
  ...(mode === 'esm' ? { transform: {} } : {}),
}

if (mode === 'optout') {
  const { reporters, runtime, ...config } = module.exports
  module.exports = { reporters, testSequencer: path.join(fixtures, 'support/sequencer.js'), projects: [
    { ...config, runtime, testMatch: [path.join(fixtures, 'io.js')] },
    { ...config, testEnvironment: 'node', setupFilesAfterEnv: [], testMatch: [path.join(fixtures, 'optout/*.js')] },
  ] }
}
