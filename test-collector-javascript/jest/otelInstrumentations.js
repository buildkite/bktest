const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const { describeError } = require('../util/otel/warn')

const PREFIX = '@opentelemetry/instrumentation-'
const SDK_LINES = { http: [200, 300], undici: [11, 34] }
const listedIn = (variable, name) => (process.env[variable] || '').split(',').some((entry) => entry.trim() === name)
// Match auto-instrumentations-node's default exclusions, even for packages
// discovered only by scanning node_modules.
const enabledByEnv = (name) => (process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS
  ? listedIn('OTEL_NODE_ENABLED_INSTRUMENTATIONS', name)
  : !['fs', 'host-metrics'].includes(name)) &&
  !listedIn('OTEL_NODE_DISABLED_INSTRUMENTATIONS', name)

// Resolve from the project first, then the auto-instrumentations bundle's
// dependencies (which pnpm does not hoist). Discover, rather than allowlist,
// optional instrumentation packages visible from either location.
const sets = new Map()
const loadInstrumentations = (rootDir, warnOnce) => {
  if (sets.has(rootDir)) return sets.get(rootDir)
  const project = createRequire(path.join(rootDir, 'package.json'))
  const loaders = [project]
  const manifests = [path.join(rootDir, 'package.json')]
  try {
    const auto = project.resolve('@opentelemetry/auto-instrumentations-node')
    loaders.push(createRequire(auto))
    // The bundle does not export package.json. Read its dependency names
    // from disk as well, so discovery does not require node_modules (PnP).
    let directory = path.dirname(auto)
    while (!fs.existsSync(path.join(directory, 'package.json')) && directory !== path.dirname(directory)) directory = path.dirname(directory)
    manifests.push(path.join(directory, 'package.json'))
  } catch (error) {
    if (error.code !== 'MODULE_NOT_FOUND') throw error
  }
  const names = new Set(Object.keys(SDK_LINES).map((name) => PREFIX + name))
  for (const file of manifests) {
    try {
      const manifest = JSON.parse(fs.readFileSync(file, 'utf8'))
      for (const name of Object.keys({ ...manifest.dependencies, ...manifest.devDependencies, ...manifest.optionalDependencies })) names.add(name)
    } catch (error) {
      if (error.code !== 'ENOENT') throw error // Jest rootDir need not contain a manifest.
    }
  }
  for (const load of loaders) {
    for (const directory of load.resolve.paths(PREFIX + 'http') || []) {
      try {
        for (const name of fs.readdirSync(path.join(directory, '@opentelemetry'))) names.add(`@opentelemetry/${name}`)
      } catch (error) {
        if (error.code !== 'ENOENT' && error.code !== 'ENOTDIR') throw error
      }
    }
  }
  const entries = []
  const skipped = []
  for (const packageName of names) {
    if (!packageName.startsWith(PREFIX)) continue
    const name = packageName.slice(PREFIX.length)
    if (!enabledByEnv(name)) continue
    const rejected = []
    for (const load of loaders) {
      let resolved
      try { resolved = load.resolve(packageName) } catch (error) {
        if (error.code !== 'MODULE_NOT_FOUND') rejected.push(`${packageName} (${describeError(error)})`)
        continue
      }
      try {
        const mod = load(packageName)
        const found = []
        for (const [className, Class] of Object.entries(mod)) {
          if (!className.endsWith('Instrumentation') || typeof Class !== 'function') continue
          const instance = new Class({ enabled: false })
          // Core instrumentations use their own version; contrib releases use
          // the instrumentation base they depend on to identify the SDK line.
          const version = SDK_LINES[name] ? instance.instrumentationVersion : createRequire(resolved)('@opentelemetry/instrumentation/package.json').version
          const [major, minor] = String(version).split('.').map(Number)
          const [min, max] = SDK_LINES[name] || [200, 300]
          if (major !== 0 || minor < min || minor >= max) throw new Error(`unsupported SDK line (${version})`)
          found.push({ name, resolved, className, instance })
        }
        entries.push(...found)
        rejected.length = 0
        break
      } catch (error) {
        rejected.push(`${packageName} (${describeError(error).split('\n')[0]})`)
      }
    }
    skipped.push(...rejected)
  }
  if (skipped.length) warnOnce('instrumentation', `OpenTelemetry instrumentation skipped: ${skipped.join(', ')}. ` +
    'The collector records child spans with instrumentation from the OpenTelemetry JavaScript 2.x SDK line; test.execution export remains enabled.')
  sets.set(rootDir, entries)
  return entries
}

module.exports = { loadInstrumentations }
