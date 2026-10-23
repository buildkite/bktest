// Child span export with the collector's provider (and, for OTEL_CHILD_CASE
// suite*, a provider the suite registers itself). Files without a .test.js
// suffix keep these fixtures out of the plain example runs.
const major = Number(require('jest/package.json').version.split('.')[0])
const mode = process.env.OTEL_CHILD_CASE || 'managed'

const instrumentations = {
  'bad-option': [['@opentelemetry/instrumentation-http', { requestHook: 'not JSON-safe', hook: new Date() }]],
  // With OTEL_NODE_DISABLED_INSTRUMENTATIONS=undici in the e2e test.
  'missing-package': [
    '@opentelemetry/instrumentation-not-installed',
    '@opentelemetry/instrumentation-http',
    '@opentelemetry/instrumentation-undici',
    ['@opentelemetry/instrumentation-graphql', { enabled: false }],
  ],
}[mode] || [
  '@opentelemetry/instrumentation-http',
  '@opentelemetry/instrumentation-undici',
  ['@opentelemetry/instrumentation-graphql', { mergeItems: true }],
]

module.exports = {
  reporters: ['default', ['buildkite-test-collector/jest/reporter', { otelEnabled: true, otelInstrumentations: instrumentations }]],
  testEnvironment: '<rootDir>/otel-children/environment.js',
  // Jest 27 calls the runtime option moduleLoader.
  ...(mode === 'no-runtime' ? {} : { [major >= 28 ? 'runtime' : 'moduleLoader']: 'buildkite-test-collector/jest/runtime' }),
  setupFilesAfterEnv: mode.startsWith('suite') ? ['<rootDir>/otel-children/suite.setup.js'] : [],
  testLocationInResults: true,
  testMatch: ['<rootDir>/otel-children/*.js', '!**/environment.js', '!**/*.setup.js', '!**/preload.js'],
}
