// The reporter enables OTel; the runtime exports one span per test.
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
