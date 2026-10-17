// Files that pick their own environment through the @jest-environment
// docblock: the e2e tests check each file takes the path its environment implies.
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
  testMatch: ['<rootDir>/otel-docblock/*.js']
};

module.exports = config;
