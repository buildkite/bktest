const fs = require('fs')
const os = require('os')
const path = require('path')
const vm = require('vm')
const { SETTINGS_ENV, publishSettings } = require('../util/otel/config')

jest.mock('../util/otel/session', () => ({
  sessionFor: jest.fn(() => ({ fake: true })),
  createSession: jest.fn(() => ({ shutdown: () => Promise.resolve() })),
}))
const { sessionFor } = require('../util/otel/session')
const { extend } = require('./environment')
const Reporter = require('./reporter')
const { isExported } = require('./exportRecords')

const consoleWarnSpy = jest.spyOn(console, 'warn').mockImplementation()
const warnings = () => consoleWarnSpy.mock.calls.map(([message]) => message)

class Base {
  constructor(config, context) {
    this.config = config
    this.context = context
  }
}
const Environment = extend(Base)

const exportRecords = fs.mkdtempSync(path.join(os.tmpdir(), 'bk-export-records-'))
const settings = {
  endpoint: 'http://127.0.0.1:9/v1/traces', token: 'secret', runEnv: { key: 'run-1', location_prefix: 'app' }, tags: {},
  rootDir: '/proj', exportRecords, fallback: 'uploading results as JSON instead',
}
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
  fs.rmSync(exportRecords, { recursive: true, force: true })
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

  it('makes paths relative to the reporter\'s rootDir in a Jest 27 multi-project run, as the JSON upload does', async () => {
    process.env.BUILDKITE_ANALYTICS_TOKEN = 'secret'
    const reporter = new Reporter({ rootDir: '/repo' }, { otelEnabled: true })
    const projectConfig = { rootDir: '/repo/packages/a', cwd: '/repo/packages/a' }
    const environment = new Environment(projectConfig, { testPath: '/repo/packages/a/same.test.js' })
    await reporter.onRunComplete()

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
      expect.stringMatching(/^\[buildkite-test-collector\] OpenTelemetry could not be set up in this test worker \(SyntaxError: .*\); for \/proj\/pkg\/sum\.test\.js, its results go to the JSON upload instead\.$/),
    ])
  })

  it('records the file as exported before its first span, so the reporter skips its JSON', async () => {
    publishSettings(settings)
    const session = { startTestSpan: jest.fn(), finishTestSpan: jest.fn() }
    sessionFor.mockReturnValueOnce(session)
    const environment = new Environment({ rootDir: '/proj', id: 'project-a' }, context)

    expect(isExported(exportRecords, 'project-a', context.testPath)).toBe(false)
    await environment.handleTestEvent({ name: 'setup' }, {})
    expect(isExported(exportRecords, 'project-a', context.testPath)).toBe(true)
    expect(isExported(exportRecords, 'project-b', context.testPath)).toBe(false)
  })

  it('exports nothing from a file it cannot record, leaving it to the JSON upload', async () => {
    publishSettings({ ...settings, exportRecords: path.join(exportRecords, 'missing', 'dir') })
    const session = { startTestSpan: jest.fn(), finishTestSpan: jest.fn() }
    sessionFor.mockReturnValueOnce(session)
    const environment = new Environment({ rootDir: '/proj', id: 'project-a' }, context)
    const test = { name: 'sums', parent: { name: 'ROOT_DESCRIBE_BLOCK' }, errors: [], asyncError: new Error() }

    await environment.handleTestEvent({ name: 'test_start', test }, {})
    await environment.handleTestEvent({ name: 'test_done', test }, {})

    expect(session.startTestSpan).not.toHaveBeenCalled()
    expect(warnings()).toEqual([
      expect.stringMatching(/^\[buildkite-test-collector\] OpenTelemetry could not record app\/pkg\/sum\.test\.js as exported \(.*ENOENT.*\); uploading results as JSON instead\.$/),
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

// A project whose jest-circus is `version`, with a jest-message-util beside it
// whose source is `messageUtil`; returns the project config a worker gets.
const projects = []
const projectWithJestCircus = (version, messageUtil) => {
  const rootDir = fs.mkdtempSync(path.join(os.tmpdir(), 'bk-jest-circus-'))
  projects.push(rootDir)
  const circus = path.join(rootDir, 'node_modules', 'jest-circus')
  const util = path.join(rootDir, 'node_modules', 'jest-message-util')
  fs.mkdirSync(path.join(circus, 'build'), { recursive: true })
  fs.mkdirSync(util, { recursive: true })
  fs.writeFileSync(path.join(circus, 'package.json'), JSON.stringify({ name: 'jest-circus', version }))
  fs.writeFileSync(path.join(circus, 'build', 'runner.js'), '')
  fs.writeFileSync(path.join(util, 'package.json'), JSON.stringify({ name: 'jest-message-util', version }))
  fs.writeFileSync(path.join(util, 'index.js'), messageUtil)
  return { rootDir, testRunner: path.join(circus, 'build', 'runner.js') }
}
afterAll(() => projects.forEach((dir) => fs.rmSync(dir, { recursive: true, force: true })))

// Calls the environment's handler from a frame in `project`'s jest-circus, as
// that jest-circus dispatches events when it runs a test file.
const dispatcherIn = (project) => vm.runInThisContext('(handler, event) => handler(event, {})', {
  filename: path.join(path.dirname(project.testRunner), 'jestAdapterInit.js'),
})

// Stands in for Jest 30.5's flattenErrorStack, which appends each cause.
const FLATTEN_ERROR_STACK = `
const flattenErrorStack = (error) => typeof error === 'string' ? error : error.stack + (error.cause ? '\\n\\n[cause]: ' + flattenErrorStack(error.cause) : '')
module.exports = { flattenErrorStack }
`

const errorWith = (message, stack, cause) => Object.assign(new Error(message, cause === undefined ? undefined : { cause }), { stack })
const outer = () => errorWith('outer', 'Error: outer\n    at a (/repo/fail.test.js:1:1)',
  errorWith('middle', 'Error: middle\n    at b (/repo/fail.test.js:2:1)', 'bottom'))
const asyncError = () => errorWith('', 'Error\n    at Object.test (/repo/fail.test.js:3:1)')

const FAILURES = [
  ['Jest 29 ignores causes', '29.7.0', '', [outer(), asyncError()],
    'Error: outer\n    at a (/repo/fail.test.js:1:1)'],
  ['Jest 30.3 falls back to the message for an empty stack', '30.3.0', '', [errorWith('empty', ''), asyncError()],
    'empty'],
  ['Jest 30.4 appends causes', '30.4.1', '', [outer(), asyncError()],
    'Error: outer\n    at a (/repo/fail.test.js:1:1)\n\n[cause]: Error: middle\n    at b (/repo/fail.test.js:2:1)\n\n[cause]: bottom'],
  ['Jest 30.5 flattens errors with its own flattenErrorStack', '30.5.2', FLATTEN_ERROR_STACK, [outer(), asyncError()],
    'Error: outer\n    at a (/repo/fail.test.js:1:1)\n\n[cause]: Error: middle\n    at b (/repo/fail.test.js:2:1)\n\n[cause]: bottom'],
  ['Jest 30.5 wraps a non-Error throw in an Error caused by the test\'s own', '30.5.2', FLATTEN_ERROR_STACK, ['oops', asyncError()],
    'Error: thrown: "oops"\n\n[cause]: Error\n    at Object.test (/repo/fail.test.js:3:1)'],
]

describe('BuildkiteTestEngineEnvironment failure text', () => {
  it.each(FAILURES)('%s', async (_, version, messageUtil, errors, text) => {
    publishSettings({ ...settings, rootDir: '/repo', runEnv: { key: 'run-1' } })
    const session = { startTestSpan: jest.fn(() => 'handle'), finishTestSpan: jest.fn() }
    sessionFor.mockReturnValueOnce(session)
    const project = projectWithJestCircus(version, messageUtil)
    const environment = new Environment(project, { testPath: '/repo/fail.test.js' })
    const test = { name: 'fails', parent: { name: 'ROOT_DESCRIBE_BLOCK' }, errors: [errors], asyncError: errors[1] }
    const dispatch = dispatcherIn(project)
    const handler = environment.handleTestEvent.bind(environment)

    await dispatch(handler, { name: 'test_start', test })
    await dispatch(handler, { name: 'test_done', test })

    const [, record] = session.finishTestSpan.mock.calls[0]
    expect([record.failure_reason, ...record.failure_expanded[0].expanded].join('\n')).toBe(text)
  })

  it('formats failures as the jest-circus dispatching events does, not the one beside a custom testRunner', async () => {
    publishSettings({ ...settings, rootDir: '/repo', runEnv: { key: 'run-1' } })
    const session = { startTestSpan: jest.fn(() => 'handle'), finishTestSpan: jest.fn() }
    sessionFor.mockReturnValueOnce(session)
    const host = projectWithJestCircus('30.5.2', FLATTEN_ERROR_STACK)
    const delegate = projectWithJestCircus('30.4.1', '')
    const dispatch = dispatcherIn(delegate)
    const environment = new Environment(host, { testPath: '/repo/fail.test.js' })
    const errors = ['oops', asyncError()]
    const test = { name: 'fails', parent: { name: 'ROOT_DESCRIBE_BLOCK' }, errors: [errors], asyncError: errors[1] }
    const handler = environment.handleTestEvent.bind(environment)

    await dispatch(handler, { name: 'test_start', test })
    await dispatch(handler, { name: 'test_done', test })

    // Jest 30.4 reports a thrown string at the test's call site; 30.5 would wrap it in an Error.
    const [, record] = session.finishTestSpan.mock.calls[0]
    expect([record.failure_reason, ...record.failure_expanded[0].expanded].join('\n')).toBe('Error\n    at Object.test (/repo/fail.test.js:3:1)')
  })
})
