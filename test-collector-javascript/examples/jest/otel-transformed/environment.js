const { extend } = require('buildkite-test-collector/jest/environment')
const node = require('jest-environment-node')

export default extend(node.default || node)
