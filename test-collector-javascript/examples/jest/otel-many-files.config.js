// Ten files of three tests, so a slow endpoint outlasts each worker's flush
// budget part way through the run.
const config = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', {
      otelEnabled: true,
      tags: { hello: "jest" }
    }]
  ],
  testEnvironment: 'buildkite-test-collector/jest/environment',
  testLocationInResults: true,
  testMatch: ['<rootDir>/otel-many-files/*.test.js']
};

module.exports = config;
