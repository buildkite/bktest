const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const { describeError } = require('../util/otel/warn')
const { worker } = require('./otelWorker')

const PREFIX = '@opentelemetry/instrumentation-'
const SDK_LINES = { http: [200, 300], undici: [11, 34] }
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

// Resolve from the project first, then the auto-instrumentations bundle's
// dependencies (which pnpm does not hoist). Only project/workspace manifests
// and the bundle choose packages, never incidental hoisted dependencies.
const loadInstrumentations = (rootDir, warnOnce) => {
  if (worker.sets.has(rootDir)) return worker.sets.get(rootDir)
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
    // The bundle does not export package.json. Read its dependency names
    // from disk as well, so discovery does not require node_modules (PnP).
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
  worker.sets.set(rootDir, entries)
  return entries
}

module.exports = { loadInstrumentations }
