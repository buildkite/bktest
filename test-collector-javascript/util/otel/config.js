const { inspect } = require('util')
const { warn, describeError } = require('./warn')

const DEFAULT_ENDPOINT = 'https://tests-otlp.buildkite.com/v1/traces'

// Reject invalid header values before loading the optional SDK.
const RUN_KEY_FORMAT = /^[!-~]{1,255}$/

// Workers inherit the environment, not reporter options.
const SETTINGS_ENV = 'BUILDKITE_TEST_COLLECTOR_OTEL_SETTINGS'

const isValidRunKey = (runKey) => typeof runKey === 'string' && RUN_KEY_FORMAT.test(runKey)

const isOtelEnabled = (value) => value === true || (typeof value === 'string' && value.trim().toLowerCase() === 'true')

const otlpEndpoint = () => (process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT ?? DEFAULT_ENDPOINT).trim()

const isValidEndpoint = (endpoint) => {
  try {
    const url = new URL(endpoint)
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.hostname !== ''
  } catch {
    return false
  }
}

const jsonTokenFor = (token) => (token || process.env.BUILDKITE_ANALYTICS_TOKEN || '').trim()

const fallbackOutcome = ({ token } = {}) => jsonTokenFor(token)
  ? 'uploading results as JSON instead'
  : 'results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set'

const stringTags = (tags) => {
  const result = {}
  for (const [key, value] of Object.entries(tags || {})) {
    if (value !== undefined && value !== null) result[key] = String(value)
  }
  return result
}

const buildSession = (settings) => {
  const { createSession } = require('./session')
  createSession(settings).shutdown().catch(() => {})
}

// Always publish the decision so stale parent settings cannot enable export.
const resolveSettings = (options) => {
  const settings = decideSettings(options)
  publishSettings(settings)
  return settings
}

const decideSettings = ({ otelEnabled, token, testRunner, frameworkVersion, runEnv, tags, unsupported, probe = buildSession }) => {
  if (!isOtelEnabled(otelEnabled)) return null

  const jsonToken = jsonTokenFor(token)
  const relayToken = (process.env.BUILDKITE_TESTS_OTLP_TOKEN || '').trim()
  const credential = relayToken || jsonToken
  if (!credential) return null

  if (unsupported) {
    warn(`otelEnabled is set, but ${unsupported}; ${fallbackOutcome({ token })}`)
    return null
  }

  const couldNotConfigure = () => {
    warn(`otelEnabled is set, but OpenTelemetry could not be configured (see the warning above); ${fallbackOutcome({ token })}`)
    return null
  }

  if (!isValidRunKey(runEnv.key)) {
    warn(
      `OpenTelemetry span export disabled: run key ${inspect(runEnv.key)} is invalid. ` +
      'Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); ' +
      'it must be 1-255 printable ASCII characters without spaces.'
    )
    return couldNotConfigure()
  }

  const endpoint = otlpEndpoint()
  if (!isValidEndpoint(endpoint)) {
    warn(
      `OpenTelemetry span export disabled: endpoint ${inspect(endpoint)} is invalid. ` +
      'BUILDKITE_ANALYTICS_OTLP_ENDPOINT must be an http or https URL with a host.'
    )
    return couldNotConfigure()
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
    warn(`OpenTelemetry span export disabled: ${describeError(error)}`)
    return couldNotConfigure()
  }

  return settings
}

const publishSettings = (settings) => {
  if (settings) {
    process.env[SETTINGS_ENV] = JSON.stringify(settings)
  } else {
    delete process.env[SETTINGS_ENV]
  }
}

const readSettings = () => {
  const json = process.env[SETTINGS_ENV]
  return json ? JSON.parse(json) : null
}

module.exports = {
  DEFAULT_ENDPOINT,
  SETTINGS_ENV,
  isOtelEnabled,
  isValidRunKey,
  fallbackOutcome,
  resolveSettings,
  publishSettings,
  readSettings,
}
