const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const semver = require('semver')
const { describeError } = require('./warn')
const { packageVersion } = require('./sdk')
const sets = new Map()

const PREFIX = '@opentelemetry/instrumentation-'
// Contrib versions don't identify the SDK line; check their base instead.
const MINIMUM_VERSIONS = { http: '0.200.0', undici: '0.11.0' }
const optionalVersion = (name, load) => {
  try { return packageVersion(name, load) } catch { return undefined }
}
const DEFAULT_EXCLUDED = [
  // auto-instrumentations-node's default exclusions.
  'fs', 'host-metrics',
  // Log correlation/sending changes records the suite may assert on.
  'pino', 'winston', 'bunyan',
  // Message instrumentation changes payload/header carriers, even without propagation.
  'aws-sdk', 'kafkajs', 'amqplib',
  // Low-level connection/lookup noise beneath HTTP spans.
  'net', 'dns',
  // Metrics only: this collector exports spans.
  'runtime-node',
  // Browser globals, not Node I/O (Node fetch is covered by undici).
  'fetch', 'xml-http-request', 'document-load', 'user-interaction', 'long-task',
]
const listedIn = (variable, name) => (process.env[variable] || '').split(',').some((entry) => entry.trim() === name)
const enabledByEnv = (name) => (process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS
  ? listedIn('OTEL_NODE_ENABLED_INSTRUMENTATIONS', name)
  : !DEFAULT_EXCLUDED.includes(name)) &&
  !listedIn('OTEL_NODE_DISABLED_INSTRUMENTATIONS', name)

// Declarations opt in, not incidental hoisting from another dependency.
const loadInstrumentations = (rootDir, warnOnce, only) => {
  const key = JSON.stringify([rootDir, only])
  if (sets.has(key)) return sets.get(key)
  const project = createRequire(path.join(rootDir, 'package.json'))
  const loaders = [project]
  const manifests = []
  for (let directory = rootDir; ; directory = path.dirname(directory)) {
    manifests.push(path.join(directory, 'package.json'))
    if (directory === path.dirname(directory)) break
  }
  try {
    const auto = project.resolve('@opentelemetry/auto-instrumentations-node')
    loaders.push(createRequire(auto))
    // The bundle does not export package.json; find it without assuming node_modules.
    let directory = path.dirname(auto)
    while (!fs.existsSync(path.join(directory, 'package.json')) && directory !== path.dirname(directory)) directory = path.dirname(directory)
    manifests.push(path.join(directory, 'package.json'))
  } catch (error) {
    if (error.code !== 'MODULE_NOT_FOUND') throw error
  }
  const names = new Set()
  for (const file of manifests) {
    try {
      const manifest = JSON.parse(fs.readFileSync(file, 'utf8'))
      for (const name of Object.keys({ ...manifest.dependencies, ...manifest.devDependencies, ...manifest.optionalDependencies })) names.add(name)
    } catch (error) {
      if (error.code !== 'ENOENT') throw error // Jest rootDir need not contain a manifest.
    }
  }
  const entries = []
  const skipped = []
  for (const packageName of names) {
    if (!packageName.startsWith(PREFIX)) continue
    const name = packageName.slice(PREFIX.length)
    if ((only && !only.includes(name)) || !enabledByEnv(name)) continue
    const rejected = []
    for (const load of loaders) {
      let resolved
      try { resolved = load.resolve(packageName) } catch (error) {
        if (error.code !== 'MODULE_NOT_FOUND') rejected.push(`${packageName} (${describeError(error)})`)
        continue
      }
      try {
        // Contrib versions don't identify the SDK line; their base dependency does.
        // Check before construction: base 0.300+ throws on api 1.9 (no api.logs).
        const from = createRequire(resolved)
        const base = optionalVersion('@opentelemetry/instrumentation', from)
        const apiVersion = optionalVersion('@opentelemetry/api', from)
        if (base && semver.gte(base, '0.300.0') && apiVersion && semver.lt(apiVersion, '1.10.0')) {
          throw new Error(`built on @opentelemetry/instrumentation ${base} (SDK 3 line), which needs @opentelemetry/api 1.10+, not ${apiVersion}`)
        }
        const mod = load(packageName)
        const found = []
        for (const [className, Class] of Object.entries(mod)) {
          if (!className.endsWith('Instrumentation') || typeof Class !== 'function') continue
          const instance = new Class({ enabled: false })
          const version = MINIMUM_VERSIONS[name] ? instance.instrumentationVersion : base
          if (!semver.valid(version) || semver.lt(version, MINIMUM_VERSIONS[name] || '0.200.0')) throw new Error(`unsupported SDK line (${version})`)
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
    'The collector records child spans with instrumentation from the OpenTelemetry JavaScript SDK 2.x or 3.x line matching your @opentelemetry/api; test.execution export remains enabled.')
  sets.set(key, entries)
  return entries
}

module.exports = { loadInstrumentations }
