// A test environment written with ESM `export default`, so it only loads
// through the project's transformer the way jest-runner loads it in workers.
// The e2e tests check the reporter reaches the same decision in the main process.
const config = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', {
      otelEnabled: true,
      tags: { hello: "jest" }
    }]
  ],
  testEnvironment: '<rootDir>/otel-transformed/environment.js',
  transform: { '\\.js$': '<rootDir>/otel-transformed/transform.js' },
  testLocationInResults: true,
  testMatch: ['<rootDir>/otel-transformed/*.test.js']
};

module.exports = config;
