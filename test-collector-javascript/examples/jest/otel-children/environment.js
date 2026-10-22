const Environment = require('buildkite-test-collector/jest/environment')

// Jest 27 predates Node's fetch global and does not copy it into the sandbox.
// Expose the real Node implementation, not a mocked fetch or HTTP transport.
module.exports = class extends Environment {
  constructor(config, context) {
    super(config, context)
    this.global.fetch = globalThis.fetch
  }
}
