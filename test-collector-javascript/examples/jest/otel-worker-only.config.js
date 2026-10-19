// A test environment that loads in Jest's worker processes but not in the
// main process, the way Yarn Plug'n'Play can leave the reporter unable to load
// what the workers run. The e2e tests check each test is still sent once.
const config = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', {
      otelEnabled: true,
      tags: { hello: "jest" }
    }]
  ],
  testEnvironment: '<rootDir>/otel-worker-only/environment.js',
  testLocationInResults: true,
  testMatch: ['<rootDir>/otel-worker-only/*.test.js']
};

module.exports = config;
