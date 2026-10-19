if (typeof process.send !== 'function') {
  throw new Error('this test environment only loads in a Jest worker process')
}

module.exports = require('buildkite-test-collector/jest/environment')
