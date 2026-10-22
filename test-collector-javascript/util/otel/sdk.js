const { createRequire } = require('module')
const { peerDependencies } = require('../../package.json')
const { describeError } = require('./warn')

// Keyed by the name the session uses for each module.
const PACKAGES = {
  api: '@opentelemetry/api',
  trace: '@opentelemetry/sdk-trace-base',
  resources: '@opentelemetry/resources',
  exporter: '@opentelemetry/exporter-trace-otlp-proto',
}

const installCommand = () => {
  const specs = Object.values(PACKAGES).map((name) => `"${name}@${peerDependencies[name]}"`)
  return `npm install --save-dev ${specs.join(' ')}`
}

// The OpenTelemetry packages are optional peer dependencies: they are only
// required here, once OTel mode has been requested and the credential and run
// key have been accepted, never at package load.
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
  return sdk
}

// Packages from different OpenTelemetry releases load and create spans, then
// fail every export (an SDK 1.x span has no instrumentationScope for a 0.2xx
// exporter to read, and the reverse), so a span from a provider built with
// `providerConfig` is serialized here with the exporter's own transformer
// before any test runs.
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

module.exports = { loadSdk, assertCanExport }
