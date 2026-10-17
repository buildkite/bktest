const { defineConfig } = require('cypress')

module.exports = defineConfig({
  reporter: '../../cypress/reporter',
  reporterOptions: {
    otelEnabled: true,
    token_name: 'BUILDKITE_ANALYTICS_CYPRESS_TOKEN',
    tags: { hello: 'cypress' },
  },
  e2e: { supportFile: false, specPattern: 'otel/*.spec.js' },
  screenshotOnRunFailure: false,
  video: false,
})
