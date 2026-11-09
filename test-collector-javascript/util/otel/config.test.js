const { createRequire } = require('module')
const { resolveSettings, publishSettings, readSettings } = require('./config')

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
const installHint = require('semver').gte(require('./sdk').packageVersion('@opentelemetry/api'), '1.10.0')
  ? 'npm install --save-dev "@opentelemetry/api@^1.10" "@opentelemetry/sdk-trace@^3" "@opentelemetry/context-async-hooks@^3" "@opentelemetry/exporter-trace-otlp-proto@~0.301.0"'
  : 'npm install --save-dev "@opentelemetry/api@~1.9.0" "@opentelemetry/sdk-trace@^2.9" "@opentelemetry/context-async-hooks@^2" "@opentelemetry/exporter-trace-otlp-proto@~0.223.0"'

// SDK 3 packages export only their entry point, not package.json.
const manifestOf = (name, load = require) => {
  const fs = require('fs'), path = require('path')
  let directory = path.dirname(load.resolve(name))
  while (!fs.existsSync(path.join(directory, 'package.json')) || JSON.parse(fs.readFileSync(path.join(directory, 'package.json'), 'utf8')).name !== name) directory = path.dirname(directory)
  return path.join(directory, 'package.json')
}

const OLD_ENV = process.env

beforeEach(() => {
  process.env = { ...OLD_ENV }
  delete process.env.BUILDKITE_ANALYTICS_TOKEN
  delete process.env.BUILDKITE_TESTS_OTLP_TOKEN
  delete process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT
  publishSettings(null)
  consoleWarnSpy.mockClear()
})

afterAll(() => {
  process.env = OLD_ENV
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

  it('enables export only for boolean true', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'json-token'

    for (const otelEnabled of ['true', 'TRUE', 'false', 0, 1]) {
      expect(resolve({ otelEnabled })).toBeNull()
      expect(readSettings()).toBeNull()
    }
    expect(resolve({ otelEnabled: true })).not.toBeNull()
    expect(warnings()).toEqual([])
  })

  it('warns without probing when no credential is available', () => {
    const probe = jest.fn()

    expect(resolve({ probe })).toBeNull()
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toEqual([expect.stringMatching(/results were not uploaded: set BUILDKITE_TESTS_OTLP_TOKEN/)])
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
    expect(resolve({ token: '  \n' }).token).toBe('env-token')
    expect(warnings()).toEqual([])
  })

  it('uses the default endpoint unless BUILDKITE_ANALYTICS_OTLP_ENDPOINT is set', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve().endpoint).toBe('https://tests-otlp.buildkite.com/v1/traces')

    process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT = ' http://127.0.0.1:4318/v1/traces\n'
    expect(resolve().endpoint).toBe('http://127.0.0.1:4318/v1/traces')
  })

  it('warns once for an endpoint that is not an http(s) URL with a host, without probing the SDK', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const probe = jest.fn()

    const endpoints = ['', '   ', 'tests-otlp.buildkite.com/v1/traces', 'ftp://tests-otlp.buildkite.com/v1/traces', 'http://', 'not a url']
    for (const endpoint of endpoints) {
      process.env.BUILDKITE_ANALYTICS_OTLP_ENDPOINT = endpoint
      expect(resolve({ probe })).toBeNull()
      expect(readSettings()).toBeNull()
    }
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toHaveLength(endpoints.length)
    expect(warnings()[0]).toBe(
      "[buildkite-test-collector] OpenTelemetry span export disabled: endpoint '' is invalid. " +
      'BUILDKITE_ANALYTICS_OTLP_ENDPOINT must be an http or https URL with a host; results were not uploaded; fix the OpenTelemetry configuration and rerun (no JSON fallback).'
    )
  })

  it('carries the run environment, framework and tags into the settings', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'

    expect(resolve()).toEqual({
      endpoint: 'https://tests-otlp.buildkite.com/v1/traces',
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

    const { token, ...workerSettings } = settings
    expect(readSettings()).toEqual(workerSettings)
    expect(process.env.BUILDKITE_TEST_COLLECTOR_OTEL_SETTINGS).not.toContain(token)
    expect(readSettings().tags).toEqual({ shard: '3' })

    expect(resolve({ otelEnabled: false })).toBeNull()
    expect(readSettings()).toBeNull()
  })

  it('accepts printable ASCII run keys from 1 to 255 characters without spaces', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    for (const key of ['a', 'x'.repeat(255), '01234-abc_DEF.~!@#$%^&*()+=[]{}|;:\'",<>?/`\\']) {
      expect(resolve({ runEnv: runEnv({ key }) }).runEnv.key).toBe(key)
    }
    expect(warnings()).toEqual([])
  })

  it('warns once for an invalid run key without probing the SDK', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const probe = jest.fn()

    expect(resolve({ runEnv: runEnv({ key: 'bad key' }), probe })).toBeNull()
    expect(probe).not.toHaveBeenCalled()
    expect(warnings()).toEqual([
      expect.stringMatching(/OpenTelemetry span export disabled: run key 'bad key' is invalid/),
    ])
    for (const key of ['', 'x'.repeat(256), 'tab\there', 'ünïcode', undefined]) {
      expect(resolve({ runEnv: runEnv({ key }), probe })).toBeNull()
      expect(readSettings()).toBeNull()
    }
    expect(probe).not.toHaveBeenCalled()
  })

  it('warns and falls back to JSON when the probe fails', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const probe = () => { throw Object.assign(new Error("Cannot find module '@opentelemetry/sdk-trace-base'"), { code: 'MODULE_NOT_FOUND' }) }

    expect(resolve({ probe })).toBeNull()
    expect(warnings()).toEqual([
      expect.stringMatching(/Cannot find module '@opentelemetry\/sdk-trace-base'; results were not uploaded/),
    ])
  })

  it('falls back to JSON with the default probe when the SDK cannot be loaded', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    jest.doMock('@opentelemetry/sdk-trace', () => {
      throw Object.assign(new Error("Cannot find module '@opentelemetry/sdk-trace'"), { code: 'MODULE_NOT_FOUND' })
    })

    try {
      expect(resolve({ probe: undefined })).toBeNull()
    } finally {
      jest.dontMock('@opentelemetry/sdk-trace')
    }
    expect(warnings()).toEqual([
      "[buildkite-test-collector] OpenTelemetry span export disabled: Error: Cannot find module '@opentelemetry/sdk-trace'. " +
      `Install the OpenTelemetry packages with: ${installHint}; results were not uploaded; fix the OpenTelemetry configuration and rerun (no JSON fallback).`,
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
      `Install matching versions with: ${installHint}; results were not uploaded; fix the OpenTelemetry configuration and rerun (no JSON fallback).`,
    ])
    expect(readSettings()).toBeNull()
  })

  it('does not use API metadata as an export compatibility cap', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    jest.resetModules()
    const fromTrace = createRequire(require.resolve('@opentelemetry/sdk-trace'))
    const manifest = manifestOf('@opentelemetry/api', fromTrace)
    jest.doMock(manifest, () => ({ name: '@opentelemetry/api', version: '1.10.0' }))
    try {
      // The default probe creates and serializes a real SDK 2 span.
      const settings = resolve({ probe: undefined })
      expect(warnings()).toEqual([])
      expect(settings).toMatchObject({ token: 'env-token' })
    } finally {
      jest.dontMock(manifest)
      jest.resetModules()
    }
  })

  it.each(['missing', '1.30.1'])('exports test spans when the child context package is %s', (version) => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    jest.resetModules()
    const manifest = manifestOf('@opentelemetry/context-async-hooks')
    jest.doMock(manifest, () => {
      if (version === 'missing') throw new Error('context package is not installed')
      return { name: '@opentelemetry/context-async-hooks', version }
    })
    try {
      // The default probe must still serialize a real test span.
      expect(resolve({ probe: undefined })).toMatchObject({ token: 'env-token' })
      expect(warnings()).toEqual([])
    } finally {
      jest.dontMock(manifest)
      jest.resetModules()
    }
  })

  it.each([
    ['@opentelemetry/sdk-trace', '2.8.0'],
    ['@opentelemetry/exporter-trace-otlp-proto', '0.219.0'],
  ])('names unsupported %s %s and explains how to install compatible packages', (name, version) => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    jest.resetModules()
    jest.doMock(manifestOf(name), () => ({ name, version }))
    try {
      expect(resolve({ probe: undefined })).toBeNull()
    } finally {
      jest.dontMock(manifestOf(name))
      jest.resetModules()
    }
    expect(warnings()).toEqual([
      expect.stringContaining(`unsupported ${name}@${version}`),
    ])
    expect(warnings()[0]).toContain('Install the OpenTelemetry packages with: npm install --save-dev')
    expect(warnings()[0]).toContain('@opentelemetry/context-async-hooks')
    expect(warnings()[0]).toContain(installHint)
    expect(warnings()[0]).toContain('results were not uploaded')
    expect(readSettings()).toBeNull()
  })

  it('says results will not be uploaded when only the relay token held the credential', () => {
    process.env.BUILDKITE_TESTS_OTLP_TOKEN = 'relay-token'
    const probe = () => { throw new Error('boom') }

    expect(resolve({ probe })).toBeNull()
    expect(warnings()).toEqual([expect.stringMatching(/results were not uploaded.*no JSON fallback/)])
  })

  it.each([
    ['v22.15.0', '1.10.0', '^3'],
    ['v22.15.0', '1.9.0', '^2.9'],
    ['v20.19.0', '1.10.0', '^2.9'],
  ])('gives a compatible install hint on Node %s with API %s', (node, api, line) => {
    const descriptor = Object.getOwnPropertyDescriptor(process, 'version')
    const manifest = manifestOf('@opentelemetry/api')
    Object.defineProperty(process, 'version', { value: node })
    jest.doMock(manifest, () => ({ name: '@opentelemetry/api', version: api }))
    jest.doMock('@opentelemetry/sdk-trace', () => { throw new Error('missing SDK fixture') })
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    try {
      expect(resolve({ probe: undefined })).toBeNull()
      expect(warnings().at(-1)).toContain(`@opentelemetry/sdk-trace@${line}`)
      expect(warnings().at(-1)).toContain(line === '^3' ? '@opentelemetry/api@^1.10' : '@opentelemetry/api@~1.9.0')
    } finally {
      Object.defineProperty(process, 'version', descriptor)
      jest.dontMock(manifest)
      jest.dontMock('@opentelemetry/sdk-trace')
      jest.resetModules()
    }
  })

  it('warns but still probes and enables SDK 3 on older Node', () => {
    const descriptor = Object.getOwnPropertyDescriptor(process, 'version')
    const manifest = manifestOf('@opentelemetry/sdk-trace')
    Object.defineProperty(process, 'version', { value: 'v20.19.0' })
    jest.doMock(manifest, () => ({ name: '@opentelemetry/sdk-trace', version: '3.1.0' }))
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    try {
      expect(resolve({ probe: undefined })).toMatchObject({ token: 'env-token' })
      expect(warnings()).toEqual([expect.stringMatching(/SDK 3 requires Node.js >=22.15.0.*Export will still be attempted/)])
    } finally {
      Object.defineProperty(process, 'version', descriptor)
      jest.dontMock(manifest)
      jest.resetModules()
    }
  })
})

describe('publishSettings / readSettings', () => {
  it('round-trips the settings through the environment for test workers', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'env-token'
    const settings = resolve()

    publishSettings(settings)
    const { token, ...workerSettings } = settings
    expect(readSettings()).toEqual(workerSettings)

    publishSettings(null)
    expect(readSettings()).toBeNull()
  })
})
