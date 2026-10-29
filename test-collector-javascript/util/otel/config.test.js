const { resolveSettings, publishSettings, readSettings, isOtelEnabled, isValidRunKey, fallbackOutcome, SETTINGS_ENV, DEFAULT_ENDPOINT } = require('./config')

const consoleWarnSpy = jest.spyOn(console, 'warn').mockImplementation()

const runEnv = (overrides = {}) => ({ key: 'run-1', ci: 'buildkite', collector: 'js-buildkite-test-collector', version: '1.0.0', ...overrides })

const resolve = (overrides = {}) => resolveSettings({
  otelEnabled: true,
  testRunner: 'jest',
  frameworkVersion: '29.0.0',
  runEnv: runEnv(),
  tags: { hello: 'world' },
  probe: () => {},
  ...overrides,
})

const warnings = () => consoleWarnSpy.mock.calls.map(([message]) => message)

const OLD_ENV = process.env

beforeEach(() => {
  process.env = { ...OLD_ENV }
  delete process.env.BUILDKITE_ANALYTICS_TOKEN
  delete process.env.BUILDKITE_TESTS_OTLP_TOKEN
  delete process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT
  delete process.env[SETTINGS_ENV]
  consoleWarnSpy.mockClear()
})

afterAll(() => {
  process.env = OLD_ENV
})

describe('isValidRunKey', () => {
  it('accepts printable ASCII without spaces, from 1 to 255 characters', () => {
    expect(isValidRunKey('a')).toBe(true)
    expect(isValidRunKey('x'.repeat(255))).toBe(true)
    expect(isValidRunKey('01234-abc_DEF.~!@#$%^&*()+=[]{}|;:\'",<>?/`\\')).toBe(true)
  })

  it('rejects empty, too long, whitespace, control and non-ASCII keys', () => {
    expect(isValidRunKey('')).toBe(false)
    expect(isValidRunKey('x'.repeat(256))).toBe(false)
    expect(isValidRunKey('has space')).toBe(false)
    expect(isValidRunKey('tab\there')).toBe(false)
    expect(isValidRunKey('ünïcode')).toBe(false)
    expect(isValidRunKey(undefined)).toBe(false)
  })
})

describe('isOtelEnabled', () => {
  it('accepts only true and the string true, in any case', () => {
    expect(isOtelEnabled(true)).toBe(true)
    expect(isOtelEnabled('true')).toBe(true)
    expect(isOtelEnabled('TRUE')).toBe(true)
    expect(isOtelEnabled(' true ')).toBe(true)
  })

  it('treats every other value as off, including truthy strings and numbers', () => {
    for (const value of ['false', 'FALSE', 0, '0', 1, '1', 'yes', 'on', '', false, null, undefined, {}, []]) {
      expect(isOtelEnabled(value)).toBe(false)
    }
  })
})

describe('fallbackOutcome', () => {
  it('says JSON uploads when the reporter option or BUILDKITE_ANALYTICS_TOKEN holds a token', () => {
    expect(fallbackOutcome({ token: 'option-token' })).toBe('uploading results as JSON instead')
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    expect(fallbackOutcome()).toBe('uploading results as JSON instead')
  })

  it('says nothing uploads when only the relay token is set', () => {
    process.env.BUILDKITE_TESTS_OTLP_TOKEN = 'relay-token'
    process.env.BUILDKITE_ANALYTICS_TOKEN = '  '
    expect(fallbackOutcome({ token: '' })).toBe('results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set')
  })
})

describe('resolveSettings', () => {
  it('returns null without loading anything when otelEnabled is not set', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'json-token'
    const probe = jest.fn()

    expect(resolve({ otelEnabled: undefined, probe })).toBeNull()
    expect(resolve({ otelEnabled: false, probe })).toBeNull()
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toEqual([])
  })

  it('reads otelEnabled strictly, so a CLI string "false" stays off and "TRUE" turns on', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'json-token'

    expect(resolve({ otelEnabled: 'false' })).toBeNull()
    expect(resolve({ otelEnabled: 0 })).toBeNull()
    expect(resolve({ otelEnabled: '0' })).toBeNull()
    expect(resolve({ otelEnabled: 1 })).toBeNull()
    expect(resolve({ otelEnabled: 'true' })).not.toBeNull()
    expect(resolve({ otelEnabled: 'TRUE' })).not.toBeNull()
    expect(warnings()).toEqual([])
  })

  it('returns null silently when no credential is available', () => {
    const probe = jest.fn()

    expect(resolve({ probe })).toBeNull()
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toEqual([])
  })

  it('treats a blank relay token as unset', () => {
    process.env.BUILDKITE_TESTS_OTLP_TOKEN = '   '

    expect(resolve()).toBeNull()
  })

  it('prefers BUILDKITE_TESTS_OTLP_TOKEN, trimmed, over the JSON tokens', () => {
    process.env.BUILDKITE_TESTS_OTLP_TOKEN = ' relay-token \n'
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve({ token: 'option-token' }).token).toBe('relay-token')
  })

  it('falls back to the reporter token option, then BUILDKITE_ANALYTICS_TOKEN', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve({ token: 'option-token' }).token).toBe('option-token')
    expect(resolve().token).toBe('env-token')
  })

  it('uses the default endpoint unless BUILDKITE_ANALYTICS_OTLP_ENDPOINT is set', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve().endpoint).toBe(DEFAULT_ENDPOINT)
    expect(DEFAULT_ENDPOINT).toBe('https://tests-otlp.buildkite.com/v1/traces')

    process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT = ' http://127.0.0.1:4318/v1/traces\n'
    expect(resolve().endpoint).toBe('http://127.0.0.1:4318/v1/traces')
  })

  it('warns and falls back for an endpoint that is not an http(s) URL with a host, without probing the SDK', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const probe = jest.fn()

    const endpoints = ['', '   ', 'tests-otlp.buildkite.com/v1/traces', 'ftp://tests-otlp.buildkite.com/v1/traces', 'http://', 'not a url']
    for (const endpoint of endpoints) {
      process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT = endpoint
      expect(resolve({ probe })).toBeNull()
      expect(readSettings()).toBeNull()
    }
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toHaveLength(endpoints.length * 2)
    expect(warnings()[0]).toBe(
      "[buildkite-test-collector] OpenTelemetry span export disabled: endpoint '' is invalid. " +
      'BUILDKITE_ANALYTICS_OTLP_ENDPOINT must be an http or https URL with a host.'
    )
    expect(warnings()[1]).toMatch(/could not be configured .*uploading results as JSON instead/)
  })

  it('carries the run environment, framework and tags into the settings', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve()).toEqual({
      endpoint: DEFAULT_ENDPOINT,
      token: 'env-token',
      runEnv: runEnv(),
      tags: { hello: 'world' },
      testRunner: 'jest',
      frameworkVersion: '29.0.0',
    })
    expect(resolve({ tags: undefined }).tags).toEqual({})
  })

  it('stringifies tag values and drops null or undefined ones', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve({ tags: { shard: 3, big: BigInt(7), flag: true, gone: null, missing: undefined } }).tags)
      .toEqual({ shard: '3', big: '7', flag: 'true' })
  })

  it('publishes the decision to the workers and clears a stale one', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const settings = resolve({ tags: { shard: 3 } })

    expect(readSettings()).toEqual(settings)
    expect(readSettings().tags).toEqual({ shard: '3' })

    expect(resolve({ otelEnabled: false })).toBeNull()
    expect(process.env[SETTINGS_ENV]).toBeUndefined()
    expect(readSettings()).toBeNull()
  })

  it('warns and falls back to JSON for an invalid run key without probing the SDK', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const probe = jest.fn()

    expect(resolve({ runEnv: runEnv({ key: 'bad key' }), probe })).toBeNull()
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toEqual([
      expect.stringMatching(/OpenTelemetry span export disabled: run key 'bad key' is invalid/),
      expect.stringMatching(/otelEnabled is set, but OpenTelemetry could not be configured .*uploading results as JSON instead/),
    ])
  })

  it('warns and falls back to JSON when the probe fails', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const probe = () => { throw Object.assign(new Error("Cannot find module '@opentelemetry/sdk-trace-base'"), { code: 'MODULE_NOT_FOUND' }) }

    expect(resolve({ probe })).toBeNull()
    expect(warnings()).toEqual([
      "[buildkite-test-collector] OpenTelemetry span export disabled: Error: Cannot find module '@opentelemetry/sdk-trace-base'",
      expect.stringMatching(/could not be configured .*uploading results as JSON instead/),
    ])
  })

  it('falls back to JSON with the default probe when the SDK cannot be loaded', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    jest.doMock('@opentelemetry/sdk-trace-base', () => {
      throw Object.assign(new Error("Cannot find module '@opentelemetry/sdk-trace-base'"), { code: 'MODULE_NOT_FOUND' })
    })

    try {
      expect(resolve({ probe: undefined })).toBeNull()
    } finally {
      jest.dontMock('@opentelemetry/sdk-trace-base')
    }
    expect(warnings()).toEqual([
      "[buildkite-test-collector] OpenTelemetry span export disabled: Error: Cannot find module '@opentelemetry/sdk-trace-base'. " +
      'Install the OpenTelemetry packages with: npm install --save-dev "@opentelemetry/api@>=1.9.0 <1.10.0" ' +
      '"@opentelemetry/sdk-trace-base@^2.0.0" "@opentelemetry/resources@^2.0.0" "@opentelemetry/exporter-trace-otlp-proto@>=0.200.0 <0.300.0"',
      expect.stringMatching(/could not be configured .*uploading results as JSON instead/),
    ])
    expect(readSettings()).toBeNull()
  })

  it('falls back to JSON with the default probe when the exporter cannot serialize the SDK spans', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    // Exporter 0.57's transformer reads the scope from instrumentationLibrary,
    // which sdk-trace-base 2.x spans no longer have.
    jest.doMock('@opentelemetry/otlp-transformer', () => ({
      ProtobufTraceSerializer: { serializeRequest: (spans) => spans.map((span) => span.instrumentationLibrary.name) },
    }))

    try {
      expect(resolve({ probe: undefined })).toBeNull()
    } finally {
      jest.dontMock('@opentelemetry/otlp-transformer')
    }
    expect(warnings()).toEqual([
      "[buildkite-test-collector] OpenTelemetry span export disabled: Error: the installed OpenTelemetry packages cannot export spans together (TypeError: Cannot read properties of undefined (reading 'name')). " +
      'Install matching versions with: npm install --save-dev "@opentelemetry/api@>=1.9.0 <1.10.0" ' +
      '"@opentelemetry/sdk-trace-base@^2.0.0" "@opentelemetry/resources@^2.0.0" "@opentelemetry/exporter-trace-otlp-proto@>=0.200.0 <0.300.0"',
      expect.stringMatching(/could not be configured .*uploading results as JSON instead/),
    ])
    expect(readSettings()).toBeNull()
  })

  it('says results will not be uploaded when only the relay token held the credential', () => {
    process.env.BUILDKITE_TESTS_OTLP_TOKEN = 'relay-token'
    const probe = () => { throw new Error('boom') }

    expect(resolve({ probe })).toBeNull()
    expect(warnings()[1]).toMatch(/results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set/)
  })
})

describe('publishSettings / readSettings', () => {
  it('round-trips the settings through the environment for test workers', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const settings = resolve()

    publishSettings(settings)
    expect(readSettings()).toEqual(settings)

    publishSettings(null)
    expect(process.env[SETTINGS_ENV]).toBeUndefined()
    expect(readSettings()).toBeNull()
  })
})
