const { builtinModules, isBuiltin: nodeIsBuiltin } = require('module')

// Shared by every framework integration: the user names instrumentation
// packages, never instances, so the option survives being passed as JSON from
// the main process to test workers.
const UPSTREAM_PREFIX = '@opentelemetry/instrumentation-'
const DISABLED_ENV = 'OTEL_NODE_DISABLED_INSTRUMENTATIONS'

const isBuiltin = nodeIsBuiltin || ((name) => builtinModules.includes(name.replace(/^node:/, '')))

const isPlainJson = (value) => {
  if (value === null || ['string', 'number', 'boolean'].includes(typeof value)) return typeof value !== 'number' || Number.isFinite(value)
  if (Array.isArray(value)) return value.every(isPlainJson)
  if (typeof value === 'object' && Object.getPrototypeOf(value) === Object.prototype) return Object.values(value).every(isPlainJson)
  return false
}

// `otelInstrumentations: ['pkg', ['pkg', { ...json options }]]`, the shape of
// Jest's own `reporters` option. Throws a message suitable for a warning.
const parseInstrumentations = (value) => {
  if (value === undefined || value === null) return []
  if (!Array.isArray(value)) throw new Error('otelInstrumentations must be an array of package names')
  const seen = new Set()
  return value.map((entry) => {
    const [name, config = {}, ...rest] = Array.isArray(entry) ? entry : [entry]
    if (typeof name !== 'string' || name.trim() === '' || rest.length > 0) {
      throw new Error(`otelInstrumentations entries must be a package name or [name, options], not ${JSON.stringify(entry)}`)
    }
    if (config === null || typeof config !== 'object' || Array.isArray(config) || !isPlainJson(config)) {
      throw new Error(`options for ${name} must be a plain JSON object; functions and class instances cannot reach Jest's test workers`)
    }
    if (seen.has(name.trim())) throw new Error(`${name} is listed more than once in otelInstrumentations`)
    seen.add(name.trim())
    return { name: name.trim(), config }
  })
}

// The same switch, and the same short names, as
// @opentelemetry/auto-instrumentations-node: OTEL_NODE_DISABLED_INSTRUMENTATIONS=http,graphql.
const disabledByEnv = () => new Set((process.env[DISABLED_ENV] || '')
  .split(',').map((suffix) => suffix.trim()).filter(Boolean).map((suffix) => `${UPSTREAM_PREFIX}${suffix}`))

const enabledInstrumentations = (entries) => {
  const disabled = disabledByEnv()
  return entries.filter(({ name, config }) => config.enabled !== false && !disabled.has(name))
}

// Instrumentation packages export one class built on @opentelemetry/instrumentation.
const instrumentationClass = (mod, name) => {
  const classes = new Set(Object.values(mod || {}).filter((value) => typeof value === 'function' &&
    ['enable', 'disable', 'setTracerProvider'].every((method) => typeof value.prototype?.[method] === 'function')))
  if (classes.size !== 1) throw new Error(`${name} does not export exactly one OpenTelemetry instrumentation class`)
  return [...classes][0]
}

// Always constructed disabled: nothing is patched until the collector decides
// that it owns the provider, after the suite's own setup has run.
const createInstrumentation = (requireModule, { name, config }) => {
  const Instrumentation = instrumentationClass(requireModule(name), name)
  const { enabled, ...options } = config
  return new Instrumentation({ ...options, enabled: false })
}

// The modules an instrumentation patches, through @opentelemetry/instrumentation's
// public (experimental) getModuleDefinitions().
const patchedModules = (instrumentation) => {
  const definitions = typeof instrumentation.getModuleDefinitions === 'function' ? instrumentation.getModuleDefinitions() : null
  if (definitions === null || definitions === undefined) return null
  return (Array.isArray(definitions) ? definitions : [definitions]).map((definition) => definition.name)
}

module.exports = {
  isBuiltin,
  parseInstrumentations,
  enabledInstrumentations,
  createInstrumentation,
  patchedModules,
}
