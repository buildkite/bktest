const { createRequire } = require('module')
const { describeError } = require('./warn')

// Keyed by the name the session uses for each module.
const PACKAGES = {
  api: '@opentelemetry/api',
  trace: '@opentelemetry/sdk-trace-base',
  resources: '@opentelemetry/resources',
  exporter: '@opentelemetry/exporter-trace-otlp-proto',
}

// The OpenTelemetry JavaScript 2.x SDK line, which the collector exports
// with. The peer dependencies only set minimums, so that no other
// OpenTelemetry user in a project fails to install or is downgraded;
// assertCanExport decides whether the installed packages work.
const SUPPORTED_VERSIONS = {
  '@opentelemetry/api': '>=1.9.0 <1.10.0',
  '@opentelemetry/sdk-trace-base': '^2.0.0',
  '@opentelemetry/resources': '^2.0.0',
  '@opentelemetry/exporter-trace-otlp-proto': '>=0.200.0 <0.300.0',
  '@opentelemetry/context-async-hooks': '^2.0.0',
}

const installCommand = () => {
  const specs = Object.values(PACKAGES).map((name) => `"${name}@${SUPPORTED_VERSIONS[name]}"`)
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
  // W3C propagators; a dependency of the trace SDK rather than a peer of ours.
  sdk.core = createRequire(require.resolve(PACKAGES.trace))('@opentelemetry/core')
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

module.exports = { loadSdk, assertCanExport, SUPPORTED_VERSIONS }
