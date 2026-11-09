const { inspect } = require('util')
const { warn, describeError } = require('./warn')

const DEFAULT_ENDPOINT = 'https://tests-otlp.buildkite.com/v1/traces'

// Reject invalid header values before loading the optional SDK.
const RUN_KEY_FORMAT = /^[!-~]{1,255}$/

// Workers inherit the environment, not reporter options.
const SETTINGS_ENV = 'BUILDKITE_TEST_COLLECTOR_OTEL_SETTINGS'

const isValidRunKey = (runKey) => typeof runKey === 'string' && RUN_KEY_FORMAT.test(runKey)

const otlpEndpoint = () => (process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT ?? DEFAULT_ENDPOINT).trim()

const isValidEndpoint = (endpoint) => {
  try {
    const url = new URL(endpoint)
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.hostname !== ''
  } catch {
    return false
  }
}

const credentialFor = (token) => (process.env.BUILDKITE_TESTS_OTLP_TOKEN || '').trim() ||
  (token || '').trim() || (process.env.BUILDKITE_ANALYTICS_TOKEN || '').trim()

const NOT_UPLOADED = 'results were not uploaded; fix the OpenTelemetry configuration and rerun (no JSON fallback)'

const stringTags = (tags) => {
  const result = {}
  for (const [key, value] of Object.entries(tags || {})) {
    if (value !== undefined && value !== null) result[key] = String(value)
  }
  return result
}

const buildSession = () => {
  const { loadSdk, assertCanExport } = require('./sdk')
  const sdk = loadSdk()
  assertCanExport(sdk, { sampler: new sdk.trace.AlwaysOnSampler() })
}

// Always publish the decision so stale parent settings cannot enable export.
const resolveSettings = (options) => {
  const settings = decideSettings(options)
  publishSettings(settings)
  return settings
}

const decideSettings = ({ otelEnabled, token, testRunner, frameworkVersion, runEnv, tags, unsupported, probe = buildSession }) => {
  if (otelEnabled !== true) return null

  const credential = credentialFor(token)
  if (!credential) {
    warn('otelEnabled is set, but results were not uploaded: set BUILDKITE_TESTS_OTLP_TOKEN, reporter token, or BUILDKITE_ANALYTICS_TOKEN and rerun (no JSON fallback).')
    return null
  }

  if (unsupported) {
    warn(`otelEnabled is set, but ${unsupported}; ${NOT_UPLOADED}`)
    return null
  }

  if (!isValidRunKey(runEnv.key)) {
    warn(
      `OpenTelemetry span export disabled: run key ${inspect(runEnv.key)} is invalid. ` +
      'Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); ' +
      `it must be 1-255 printable ASCII characters without spaces; ${NOT_UPLOADED}.`
    )
    return null
  }

  const endpoint = otlpEndpoint()
  if (!isValidEndpoint(endpoint)) {
    warn(
      `OpenTelemetry span export disabled: endpoint ${inspect(endpoint)} is invalid. ` +
      `BUILDKITE_ANALYTICS_OTLP_ENDPOINT must be an http or https URL with a host; ${NOT_UPLOADED}.`
    )
    return null
  }

  const settings = {
    endpoint,
    token: credential,
    runEnv,
    tags: stringTags(tags),
    testRunner,
    frameworkVersion,
  }

  try {
    probe(settings)
  } catch (error) {
    warn(`OpenTelemetry span export disabled: ${describeError(error)}; ${NOT_UPLOADED}.`)
    return null
  }

  return settings
}

const publishSettings = (settings) => {
  if (settings) {
    const { token, ...workerSettings } = settings
    process.env[SETTINGS_ENV] = JSON.stringify(workerSettings)
  } else {
    delete process.env[SETTINGS_ENV]
  }
}

const readSettings = () => {
  const json = process.env[SETTINGS_ENV]
  return json ? JSON.parse(json) : null
}

module.exports = {
  credentialFor,
  resolveSettings,
  publishSettings,
  readSettings,
}
