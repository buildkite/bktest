module.exports = {
  reporters: ['default', ['buildkite-test-collector/jest/reporter', { otelEnabled: true }]],
  testEnvironment: '<rootDir>/otel-children/environment.js',
  testLocationInResults: true,
  setupFilesAfterEnv: ['<rootDir>/otel-children/setup.js'],
  testMatch: ['<rootDir>/otel-children/{io,empty}.js'],
}
