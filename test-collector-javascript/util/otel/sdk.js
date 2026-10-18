const { peerDependencies } = require('../../package.json')

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

module.exports = { loadSdk }
