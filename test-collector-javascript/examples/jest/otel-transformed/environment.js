const { extend } = require('buildkite-test-collector/jest/environment')
const NodeEnvironment = require('jest-environment-node').default

export default extend(NodeEnvironment)
