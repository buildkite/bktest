const { createRequire } = require('module')
const fs = require('fs')
const path = require('path')
const semver = require('semver')
const { randomBytes } = require('crypto')
const { resourceAttributes } = require('./attributes')
const { describeError } = require('./warn')

const PACKAGES = {
  trace: '@opentelemetry/sdk-trace-base',
  context: '@opentelemetry/context-async-hooks',
  exporter: '@opentelemetry/exporter-trace-otlp-proto',
}

// The newest-SDK CI leg also uses these ranges.
const SUPPORTED_VERSIONS = {
  '@opentelemetry/sdk-trace-base': '^2',
  '@opentelemetry/context-async-hooks': '^2',
  '@opentelemetry/exporter-trace-otlp-proto': '>=0.200.0 <0.300.0',
}

const installCommand = () => 'npm install --save-dev "@opentelemetry/sdk-trace-base@^2" ' +
  '"@opentelemetry/context-async-hooks@^2" "@opentelemetry/exporter-trace-otlp-proto@<0.300"'

const packageVersion = (name, load = require) => {
  for (let directory = path.dirname(load.resolve(name)); directory !== path.dirname(directory); directory = path.dirname(directory)) {
    const file = path.join(directory, 'package.json')
    if (fs.existsSync(file) && require(file).name === name) return require(file).version
  }
}

// Optional packages must not load until OpenTelemetry is requested.
const loadSdk = ({ exporter = true, context = false } = {}) => {
  const sdk = {}
  try {
    for (const [key, name] of Object.entries(PACKAGES)) {
      if (key === 'context' && !context) continue
      const range = SUPPORTED_VERSIONS[name]
      const version = packageVersion(name)
      if (version && !semver.satisfies(version, range)) throw new Error(`unsupported ${name}@${version} (requires ${range})`)
      if (key !== 'exporter' || exporter) sdk[key] = require(name)
    }
    const fromTrace = createRequire(require.resolve(PACKAGES.trace))
    for (const key of ['api', 'resources', 'core']) sdk[key] = fromTrace(`@opentelemetry/${key}`)
  } catch (error) {
    // The first line names the module; the rest is the require stack.
    throw new Error(`${String(error.message).split('\n')[0]}. Install the OpenTelemetry packages with: ${installCommand()}`)
  }
  return sdk
}

const SPAN_LIMITS = {
  attributeCountLimit: 128, attributeValueLengthLimit: 2 * 16384,
  eventCountLimit: 100, attributePerEventCountLimit: 128,
  linkCountLimit: 128, attributePerLinkCountLimit: 128,
}

// Suites can stub Math.random. Never let that produce invalid all-zero IDs.
const randomHex = (bytes) => {
  for (;;) {
    const hex = randomBytes(bytes).toString('hex')
    if (/[^0]/.test(hex)) return hex
  }
}

const createResource = ({ resources }, runEnv) => resources.defaultResource()
  .merge(resources.detectResources({ detectors: [resources.envDetector] }))
  .merge(resources.resourceFromAttributes(resourceAttributes(runEnv)))

// Only test.execution ignores environment-selected sampling and span limits.
// Child providers deliberately retain BasicTracerProvider's environment config.
const createProvider = (sdk, { testSpans = false, getIds = () => null, ...config } = {}) => new sdk.trace.BasicTracerProvider({
  idGenerator: {
    generateTraceId: () => getIds()?.traceId || randomHex(16),
    generateSpanId: () => getIds()?.spanId || randomHex(8),
  },
  ...(testSpans ? { sampler: new sdk.trace.AlwaysOnSampler(), spanLimits: SPAN_LIMITS } : {}),
  ...config,
})

const createContextManager = (sdk = loadSdk({ exporter: false, context: true })) => new sdk.context.AsyncLocalStorageContextManager().enable()

const createPropagator = (sdk) => new sdk.core.CompositePropagator({
  propagators: [new sdk.core.W3CTraceContextPropagator(), new sdk.core.W3CBaggagePropagator()],
})

// Mixed SDK lines can create spans but fail serialization; probe before tests run.
const assertCanExport = (sdk, providerConfig) => {
  try {
    const exporterRequire = createRequire(require.resolve(PACKAGES.exporter))
    const { ProtobufTraceSerializer } = exporterRequire('@opentelemetry/otlp-transformer')
    const span = createProvider(sdk, providerConfig).getTracer('buildkite-test-collector').startSpan('probe')
    span.end()
    ProtobufTraceSerializer.serializeRequest([span])
  } catch (error) {
    throw new Error(
      `the installed OpenTelemetry packages cannot export spans together (${describeError(error)}). ` +
      `Install matching versions with: ${installCommand()}`
    )
  }
}

module.exports = { loadSdk, createProvider, createResource, createContextManager, createPropagator, assertCanExport, SUPPORTED_VERSIONS, packageVersion }
