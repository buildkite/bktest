// Jest configured for OTel mode: the reporter decides the mode and the test
// environment exports one span per test from the worker.
const config = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', {
      otelEnabled: true,
      tags: { hello: "jest" }
    }]
  ],
  runtime: 'buildkite-test-collector/jest/runtime',
  testLocationInResults: true,
  testMatch: ['<rootDir>/*.test.js', '<rootDir>/otel/*.js']
};

module.exports = config;
