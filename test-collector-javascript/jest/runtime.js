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
    if (!this._buildkiteChildren) return
    // Newer Jest 30 releases moved the CJS loader off the runtime.
    const loader = this.cjsLoader || this
    const method = this.cjsLoader ? 'loadModule' : '_loadModule'
    const original = loader[method].bind(loader)
    loader[method] = (localModule, from, moduleName, modulePath, options, moduleRegistry) => {
      const load = () => {
        original(localModule, from, moduleName, modulePath, options, moduleRegistry)
        return localModule.exports
      }
      if (options?.isInternalModule) return load()
      // Jest has resolved aliases, conditions and mocks, and registered the
      // module for circular loads. Store replacement exports in that registry.
      localModule.exports = this._buildkiteChildren.load(modulePath, load)
    }
    this._buildkiteChildren.attachRuntime(this)
  }

  // The test file's require: tells the environment about each copy of the
  // OpenTelemetry API it loads, which jest.resetModules() can multiply. An ES
  // module's import of it arrives here as the resolved file.
  requireModuleOrMock(from, moduleName) {
    const exports = super.requireModuleOrMock(from, moduleName)
    if (/^@opentelemetry\/api$|[\\/]@opentelemetry[\\/]api[\\/]/.test(moduleName)) this._buildkiteChildren?.loadedApi(exports)
    return exports
  }
}

module.exports = extend(BaseRuntime)
module.exports.extend = extend
