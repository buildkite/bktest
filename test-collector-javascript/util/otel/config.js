const { inspect } = require('util')
const { warn, describeError } = require('./warn')

const DEFAULT_ENDPOINT = 'https://tests-otlp.buildkite.com/v1/traces'

// The receiver derives the run from this header and rejects anything else
// with a 400, so the key is checked before any OTel code is loaded.
const RUN_KEY_FORMAT = /^[!-~]{1,255}$/

// Carries the main process's decision to the test workers, which inherit the
// environment but not the reporter's options.
const SETTINGS_ENV = 'BUILDKITE_TEST_COLLECTOR_OTEL_SETTINGS'

const isValidRunKey = (runKey) => typeof runKey === 'string' && RUN_KEY_FORMAT.test(runKey)

// Reporter options can arrive as strings from a CLI (`otelEnabled=false`), so
// only `true` and the string 'true' turn OTel on; anything else is off.
const isOtelEnabled = (value) => value === true || (typeof value === 'string' && value.trim().toLowerCase() === 'true')

const otlpEndpoint = () => process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT ?? DEFAULT_ENDPOINT

const isValidEndpoint = (endpoint) => {
  try {
    const url = new URL(endpoint)
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.hostname !== ''
  } catch {
    return false
  }
}

// What happens to results when OTel is on but cannot be used: the JSON path
// runs only when it has a token of its own.
const fallbackOutcome = ({ token } = {}) => {
  const jsonToken = (token || process.env.BUILDKITE_ANALYTICS_TOKEN || '').trim()
  return jsonToken
    ? 'uploading results as JSON instead'
    : 'results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set'
}

// Tag values travel to the workers as JSON and to Test Engine as string
// attributes, so they are stringified once here.
const stringTags = (tags) => {
  const result = {}
  for (const [key, value] of Object.entries(tags || {})) {
    if (value !== undefined && value !== null) result[key] = String(value)
  }
  return result
}

// Builds and discards a session so that a missing SDK or a broken
// configuration surfaces here, before any test worker needs it.
const buildSession = (settings) => {
  const { createSession } = require('./session')
  createSession(settings).shutdown().catch(() => {})
}

// Decides, once per run, whether results go over OTLP. Returns the settings a
// session is built from, or null when the JSON path should run instead, and
// warns about every misconfiguration except a missing credential. The
// decision is published to the workers either way, so a stale value from a
// parent process cannot turn OTel on.
//
// `probe` receives the settings and must throw if a session cannot be built;
// tests replace it.
const resolveSettings = (options) => {
  const settings = decideSettings(options)
  publishSettings(settings)
  return settings
}

const decideSettings = ({ otelEnabled, token, testRunner, frameworkVersion, runEnv, tags, probe = buildSession }) => {
  if (!isOtelEnabled(otelEnabled)) return null

  const jsonToken = (token || process.env.BUILDKITE_ANALYTICS_TOKEN || '').trim()
  const relayToken = (process.env.BUILDKITE_TESTS_OTLP_TOKEN || '').trim()
  const credential = relayToken || jsonToken
  if (!credential) return null

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
