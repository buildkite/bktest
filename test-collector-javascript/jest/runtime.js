const path = require('path')
const { createRequire } = require('module')

// Extends the jest-runtime that the requiring jest-runner uses (Jest loads
// this file through the project's `runtime` option, `moduleLoader` in Jest 27),
// falling back to the project's and then this package's own copy.
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

// Lets the collector's test environment connect the file's sandbox to
// OpenTelemetry: the environment uses it to load the OpenTelemetry API and
// SDK into the sandbox's module registry, which a test environment cannot
// reach on its own. Without the collector's environment it behaves exactly
// like its base.
const extend = (Base) => class BuildkiteTestEngineRuntime extends Base {
  constructor(config, environment, ...rest) {
    super(config, environment, ...rest)
    const children = environment?._buildkite?.children
    if (children) children.attachRuntime(this)
  }
}

module.exports = extend(BaseRuntime)
module.exports.extend = extend
