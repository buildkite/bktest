const path = require('path')
const { createRequire } = require('module')

// Prefer the running jest-runner's dependencies, including under Plug'n'Play.
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

// The environment alone cannot reach Jest's sandbox module registry.
const extend = (Base) => class BuildkiteTestEngineRuntime extends Base {
  constructor(config, environment, ...rest) {
    super(config, environment, ...rest)
    const [, , , , testPath, globalConfig] = rest
    require('./otelEnvironment').attach(environment, { globalConfig, projectConfig: config }, { testPath })
    this._buildkiteChildren = environment?._buildkite?.children
    if (!this._buildkiteChildren) return
    this._buildkiteChildren.attachRuntime(this)
    if (!this._buildkiteChildren.packages.names.size) return
    // Newer Jest 30 releases moved the CJS loader off the runtime.
    const loader = this.cjsLoader || this
    const method = this.cjsLoader ? 'loadModule' : '_loadModule'
    if (typeof loader[method] !== 'function') return this._buildkiteChildren.warnOnce('loader', 'OpenTelemetry package instrumentation skipped: this Jest version has no supported module loader; test.execution export remains enabled.')
    const original = loader[method].bind(loader)
    loader[method] = (localModule, from, moduleName, modulePath, options, moduleRegistry) => {
      let originalExports
      const load = () => {
        original(localModule, from, moduleName, modulePath, options, moduleRegistry)
        return originalExports = localModule.exports
      }
      if (options?.isInternalModule) return load()
      // Preserve unchanged exports: getter-only modules can return a fresh
      // object each time and cannot accept an assignment.
      const patchedExports = this._buildkiteChildren.load(modulePath, load)
      if (patchedExports !== originalExports) localModule.exports = patchedExports
    }
  }

  // resetModules creates API copies; ESM imports arrive as resolved paths.
  requireModuleOrMock(from, moduleName) {
    const exports = super.requireModuleOrMock(from, moduleName)
    if (/^@opentelemetry\/api$|[\\/]@opentelemetry[\\/]api[\\/]/.test(moduleName)) this._buildkiteChildren?.loadedApi(exports)
    return exports
  }
}

module.exports = extend(BaseRuntime)
module.exports.extend = extend
