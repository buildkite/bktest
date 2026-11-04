// A docblock changes the environment without opting out of the runtime.
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
  testMatch: ['<rootDir>/otel-docblock/*.js']
};

module.exports = config;
