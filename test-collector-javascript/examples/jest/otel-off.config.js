// The same test files as otel.config.js, uploaded as JSON: the e2e tests
// compare the identity both paths derive.
const config = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', {
      tags: { hello: "jest" }
    }]
  ],
  testLocationInResults: true,
  testMatch: ['<rootDir>/*.test.js', '<rootDir>/otel/*.js']
};

module.exports = config;
