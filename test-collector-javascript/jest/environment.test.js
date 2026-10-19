const { SETTINGS_ENV, publishSettings } = require('../util/otel/config')

jest.mock('../util/otel/session', () => ({
  sessionFor: jest.fn(() => ({ fake: true })),
  createSession: jest.fn(() => ({ shutdown: () => Promise.resolve() })),
}))
const { sessionFor } = require('../util/otel/session')
const { extend } = require('./environment')
const Reporter = require('./reporter')

const consoleWarnSpy = jest.spyOn(console, 'warn').mockImplementation()
const warnings = () => consoleWarnSpy.mock.calls.map(([message]) => message)

class Base {
  constructor(config, context) {
    this.config = config
    this.context = context
  }
}
const Environment = extend(Base)

const settings = { endpoint: 'http://127.0.0.1:9/v1/traces', token: 'secret', runEnv: { key: 'run-1', location_prefix: 'app' }, tags: {}, rootDir: '/proj' }
const context = { testPath: '/proj/pkg/sum.test.js' }

const OLD_ENV = process.env

beforeEach(() => {
  process.env = { ...OLD_ENV }
  delete process.env[SETTINGS_ENV]
  consoleWarnSpy.mockClear()
  sessionFor.mockClear()
})

afterAll(() => {
  process.env = OLD_ENV
})

describe('BuildkiteTestEngineEnvironment constructor', () => {
  it('accepts the Jest 27 config shape (the project config alone)', () => {
    publishSettings(settings)
    // Paths are relative to rootDir, as the reporter's are, not to the worker's cwd.
    const environment = new Environment({ rootDir: '/proj', cwd: '/elsewhere' }, context)

    expect(environment.config).toEqual({ rootDir: '/proj', cwd: '/elsewhere' })
    expect(environment._buildkite.testPath).toBe('app/pkg/sum.test.js')
    expect(sessionFor).toHaveBeenCalledWith(settings)
    expect(environment._buildkite.session).toEqual({ fake: true })
    expect(warnings()).toEqual([])
  })

  it('makes paths relative to the reporter\'s rootDir in a Jest 27 multi-project run, as the JSON upload does', () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'secret'
    new Reporter({ rootDir: '/repo' }, { otelEnabled: true })
    const projectConfig = { rootDir: '/repo/packages/a', cwd: '/repo/packages/a' }
    const environment = new Environment(projectConfig, { testPath: '/repo/packages/a/same.test.js' })

    expect(environment._buildkite.testPath).toBe('packages/a/same.test.js')
    expect(warnings()).toEqual([])
  })

  it('accepts the Jest 28+ config shape ({ globalConfig, projectConfig })', () => {
    publishSettings(settings)
    const config = { globalConfig: { rootDir: '/proj' }, projectConfig: { rootDir: '/proj/pkg' } }
    const environment = new Environment(config, context)

    expect(environment._buildkite.testPath).toBe('app/pkg/sum.test.js')
    expect(environment._buildkite.session).toEqual({ fake: true })
    expect(warnings()).toEqual([])
  })

  it('runs the tests as the base environment would when OTel mode is off', async () => {
    const environment = new Environment({ rootDir: '/proj' }, context)

    expect(environment._buildkite.session).toBeNull()
    expect(sessionFor).not.toHaveBeenCalled()
    await expect(environment.handleTestEvent({ name: 'test_start', test: {} }, {})).resolves.toBeUndefined()
    expect(warnings()).toEqual([])
  })

  it('still constructs, and names the file, when OTel cannot be set up in the worker', async () => {
    process.env[SETTINGS_ENV] = '{not json'
    const environment = new Environment({ rootDir: '/proj' }, context)

    expect(environment._buildkite.session).toBeNull()
    await expect(environment.handleTestEvent({ name: 'test_start', test: {} }, {})).resolves.toBeUndefined()
    expect(warnings()).toEqual([
      expect.stringMatching(/^\[buildkite-test-collector\] TEST RESULTS MISSING: OpenTelemetry could not be set up in this test worker \(SyntaxError: .*\); test executions from \/proj\/pkg\/sum\.test\.js were not uploaded/),
    ])
  })
})

// Stacks of test.asyncError captured from real runs; `line` is the location
// Jest itself reports for that test in the JSON upload.
const stack = (...frames) => ['Error', ...frames.map((frame) => `    at ${frame}`)].join('\n')
const STACKS = [
  ['Jest 27 test.each', 1, stack(
    '/repo/node_modules/jest-each/build/bind.js:43:11',
    'Array.forEach (<anonymous>)',
    'eachBind (/repo/node_modules/jest-each/build/bind.js:42:22)',
    'Object.<anonymous> (/repo/each.test.js:1:36)',
    'Runtime._execModule (/repo/node_modules/jest-runtime/build/index.js:1646:24)',
  )],
  ['Jest 29 test.each', 1, stack(
    '/repo/node_modules/jest-each/build/bind.js:47:15',
    'Array.forEach (<anonymous>)',
    'eachBind (/repo/node_modules/jest-each/build/bind.js:39:22)',
    'Object.<anonymous> (/repo/each.test.js:1:36)',
    'Runtime._execModule (/repo/node_modules/jest-runtime/build/index.js:1439:24)',
  )],
  ['Jest 30 test.each', 1, stack(
    'eachBind (/repo/node_modules/jest-each/build/index.js:54:13)',
    'Object.<anonymous> (/repo/each.test.js:1:15)',
    'ModuleExecutor.exec (/repo/node_modules/jest-runtime/build/index.js:3055:26)',
    'CjsLoader.loadModule (/repo/node_modules/jest-runtime/build/index.js:440:36)',
    'CjsLoader.requireModule (/repo/node_modules/jest-runtime/build/index.js:335:12)',
  )],
  ['Jest 30 test.each inside describe.each', 2, stack(
    'eachBind (/repo/node_modules/jest-each/build/index.js:54:13)',
    '/repo/each.test.js:2:17',
    '/repo/node_modules/jest-each/build/index.js:80:140',
    '_dispatchDescribe (/repo/node_modules/jest-circus/build/jestAdapterInit.js:626:26)',
    'describe (/repo/node_modules/jest-circus/build/jestAdapterInit.js:594:44)',
  )],
  ['Jest 30 test inside describe.each', 3, stack(
    'test (/repo/each.test.js:3:3)',
    '/repo/node_modules/jest-each/build/index.js:80:140',
    '_dispatchDescribe (/repo/node_modules/jest-circus/build/jestAdapterInit.js:626:26)',
    'describe (/repo/node_modules/jest-circus/build/jestAdapterInit.js:594:44)',
    'eachBind (/repo/node_modules/jest-each/build/index.js:54:13)',
  )],
]

describe('BuildkiteTestEngineEnvironment test locations', () => {
  it.each(STACKS)('reports the line Jest reports for a %s', async (_, line, asyncErrorStack) => {
    publishSettings({ ...settings, rootDir: '/repo', runEnv: { key: 'run-1' } })
    const session = { startTestSpan: jest.fn(() => 'handle'), finishTestSpan: jest.fn() }
    sessionFor.mockReturnValueOnce(session)
    const environment = new Environment({ rootDir: '/repo' }, { testPath: '/repo/each.test.js' })
    const test = { name: 'each 1', parent: { name: 'ROOT_DESCRIBE_BLOCK' }, errors: [], asyncError: { stack: asyncErrorStack } }

    await environment.handleTestEvent({ name: 'include_test_location_in_result' }, {})
    await environment.handleTestEvent({ name: 'test_start', test }, {})
    await environment.handleTestEvent({ name: 'test_done', test }, {})

    const [, record, { line: spanLine }] = session.finishTestSpan.mock.calls[0]
    expect(record.location).toBe(`each.test.js:${line}`)
    expect(spanLine).toBe(line)
  })
})
