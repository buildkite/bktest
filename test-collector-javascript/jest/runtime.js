const path = require('path')
const { createRequire } = require('module')

// Extends the jest-runtime that the requiring jest-runner uses (Jest loads
// this file through the project's `runtime` option), falling back to the
// project's and then this package's own copy.
const BaseRuntime = (() => {
  const candidates = [module.parent?.filename, path.join(process.cwd(), 'package.json'), __filename].filter(Boolean)
  for (const from of candidates) {
    try {
      const mod = createRequire(from)('jest-runtime')
      return mod.default || mod
    } catch {
      // try the next location
    }
  }
  throw new Error("buildkite-test-collector/jest/runtime requires 'jest-runtime' to be installed")
})()

// Turns on child spans: the collector's test environment uses it to load the
// OpenTelemetry API and SDK into the file's sandbox, whose module registry a
// test environment cannot reach on its own. Without the collector's
// environment it behaves exactly like its base.
const extend = (Base) => class BuildkiteTestEngineRuntime extends Base {
  constructor(config, environment, ...rest) {
    super(config, environment, ...rest)
    this._buildkiteChildren = environment?._buildkite?.children
    if (this._buildkiteChildren) this._buildkiteChildren.attachRuntime(this)
  }

  // The test file's require: tells the environment about each copy of the
  // OpenTelemetry API it loads, which jest.resetModules() can multiply.
  requireModuleOrMock(from, moduleName) {
    const exports = super.requireModuleOrMock(from, moduleName)
    if (moduleName === '@opentelemetry/api') this._buildkiteChildren?.loadedApi(exports)
    return exports
  }
}

module.exports = extend(BaseRuntime)
module.exports.extend = extend
