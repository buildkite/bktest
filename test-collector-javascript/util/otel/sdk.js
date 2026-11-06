const { createRequire } = require('module')
const fs = require('fs')
const path = require('path')
const semver = require('semver')
const { describeError } = require('./warn')

const PACKAGES = {
  trace: '@opentelemetry/sdk-trace-base',
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
const loadSdk = ({ exporter = true } = {}) => {
  const sdk = {}
  try {
    for (const [key, name] of Object.entries(PACKAGES)) {
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

module.exports = { loadSdk, assertCanExport, SUPPORTED_VERSIONS, packageVersion }
