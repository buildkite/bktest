const { createRequire } = require('module')
const { describeError } = require('./warn')

const PACKAGES = {
  api: '@opentelemetry/api',
  trace: '@opentelemetry/sdk-trace-base',
  resources: '@opentelemetry/resources',
  exporter: '@opentelemetry/exporter-trace-otlp-proto',
}

// Optional peers permit JSON-only suites; export compatibility is checked below.
const SUPPORTED_VERSIONS = {
  '@opentelemetry/api': '>=1.9.0 <1.10.0',
  '@opentelemetry/sdk-trace-base': '^2.0.0',
  '@opentelemetry/resources': '^2.0.0',
  '@opentelemetry/exporter-trace-otlp-proto': '>=0.200.0 <0.300.0',
  '@opentelemetry/context-async-hooks': '^2.0.0',
}

const installCommand = () => {
  const specs = Object.entries(SUPPORTED_VERSIONS).map(([name, range]) => `"${name}@${range}"`)
  return `npm install --save-dev ${specs.join(' ')}`
}

// Optional peers must not load until OpenTelemetry is requested.
const loadSdk = () => {
  const sdk = {}
  for (const [key, name] of Object.entries(PACKAGES)) {
    try {
      sdk[key] = require(name)
    } catch (error) {
      if (error.code !== 'MODULE_NOT_FOUND') throw error
      // The first line names the module; the rest is the require stack.
      throw new Error(`${String(error.message).split('\n')[0]}. Install the OpenTelemetry packages with: ${installCommand()}`)
    }
  }
  // W3C propagators; a dependency of the trace SDK rather than a peer of ours.
  sdk.core = createRequire(require.resolve(PACKAGES.trace))('@opentelemetry/core')
  return sdk
}

// Mixed SDK lines can create spans but fail serialization; probe before tests run.
const assertCanExport = (sdk, providerConfig) => {
  try {
    const exporterRequire = createRequire(require.resolve(PACKAGES.exporter))
    const { ProtobufTraceSerializer } = exporterRequire('@opentelemetry/otlp-transformer')
    const span = new sdk.trace.BasicTracerProvider(providerConfig).getTracer('buildkite-test-collector').startSpan('probe')
    span.end()
    ProtobufTraceSerializer.serializeRequest([span])
  } catch (error) {
    throw new Error(
      `the installed OpenTelemetry packages cannot export spans together (${describeError(error)}). ` +
      `Install matching versions with: ${installCommand()}`
    )
  }
}

module.exports = { loadSdk, assertCanExport, SUPPORTED_VERSIONS }
