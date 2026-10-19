const { randomUUID } = require('crypto')
const path = require('path')
const { types } = require('util')
const Paths = require('../util/paths')
const { readSettings } = require('../util/otel/config')
const { sessionFor } = require('../util/otel/session')
const { warn, describeError } = require('../util/otel/warn')
const testEngineRecord = require('./testEngineRecord')
const { recordExport } = require('./exportRecords')

const ROOT_DESCRIBE_BLOCK_NAME = 'ROOT_DESCRIBE_BLOCK'
const STACK_LINE = /^(?:\s*at )?(?:new )?(?:.*? \()?(?:eval at [^ ]+ \(.+?:\d+:\d+\), )?(.+?):(\d+):(\d+)\)?$/
const JEST_EACH_BUILD_DIR = /[\\/]jest-each[\\/]build[\\/]/

const tryRequireFrom = (request, dirs) => {
  try {
    return require(require.resolve(request, { paths: dirs }))
  } catch {
    return null
  }
}

const stackOrMessage = (error) => (typeof error.stack === 'string' ? error.stack : error.message)
const nonEmptyStackOrMessage = (error) => (typeof error.stack === 'string' && error.stack !== '' ? error.stack : error.message)

// Jest 30.4's formatErrorStackWithCause, which jest-circus does not export.
const stackWithCauses = (isError) => {
  const format = (error, seen) => {
    const stack = nonEmptyStackOrMessage(error)
    const { cause } = error
    if (!('cause' in error) || !(typeof cause === 'string' || isError(cause) || cause instanceof Error)) return stack
    if (typeof cause === 'string') return `${stack}\n\n[cause]: ${cause}`
    if (seen.has(cause)) return `${stack}\n\n[cause]: [Circular cause]`
    seen.add(error)
    return `${stack}\n\n[cause]: ${format(cause, seen)}`
  }
  return (error) => format(error, new Set())
}

// Failures are formatted the way the jest-circus running the file formats
// them, with the modules it loads. How it turns an error into text changed in
// Jest 30.1 (an empty stack falls back to the message), 30.4 (causes are
// appended) and 30.5 (jest-message-util's flattenErrorStack, which also
// appends an AggregateError's errors, and non-Error throws are wrapped).
const failureFormatting = (projectConfig, circusDir) => {
  const dirs = [circusDir, projectConfig.testRunner && path.dirname(projectConfig.testRunner), projectConfig.rootDir, __dirname].filter(Boolean)
  const prettyFormat = tryRequireFrom('pretty-format', dirs)
  const flattenErrorStack = tryRequireFrom('jest-message-util', dirs)?.flattenErrorStack
  const circus = tryRequireFrom(circusDir ? path.join(circusDir, 'package.json') : 'jest-circus/package.json', dirs)
  const [major, minor] = (circus?.version || '0.0').split('.').map(Number)
  const since30 = (minimum) => major > 30 || (major === 30 && minor >= minimum)
  let errorText = stackOrMessage
  if (flattenErrorStack) errorText = flattenErrorStack
  else if (since30(4)) errorText = stackWithCauses(tryRequireFrom('jest-util', dirs)?.isError || types.isNativeError)
  else if (since30(1)) errorText = nonEmptyStackOrMessage
  return {
    format: prettyFormat ? (value) => prettyFormat.format(value, { maxDepth: 3 }) : String,
    errorText,
    wrapsThrownValues: Boolean(flattenErrorStack),
  }
}

// The jest-circus dispatching an event is the one running the file, even when
// a custom testRunner delegates to a copy other than the one beside it.
const JEST_CIRCUS_BUILD_FILE = /^(.*[\\/]jest-circus)[\\/]build[\\/]/
const dispatchingJestCircus = (stack) => {
  for (const line of stack.split('\n').slice(1)) {
    const match = JEST_CIRCUS_BUILD_FILE.exec(parseStackLine(line)?.file || '')
    if (match) return match[1]
  }
  return null
}

// parseStackLine through assertionResult mirror how jest-circus turns a
// finished test into the assertion result the reporter sees, so the span
// carries the same title, ancestors, location and failure text.
const parseStackLine = (line) => {
  const match = STACK_LINE.exec(line || '')
  return match ? { file: match[1], line: Number(match[2]), column: Number(match[3]) } : null
}

// Under test.each and describe.each the first frame is inside jest-each, and
// jest-circus reads the line from a fixed later frame: the fourth before
// Jest 30, the second since. Skipping every frame inside jest-each or without
// a file position lands on that frame in both.
const testLocation = (test) => {
  const frames = test.asyncError.stack.split('\n').slice(1).map(parseStackLine)
  let parsed = frames[0]
  if (parsed && JEST_EACH_BUILD_DIR.test(parsed.file)) {
    parsed = frames.slice(1).find((frame) => frame && !JEST_EACH_BUILD_DIR.test(frame.file))
  }
  return parsed ? { line: parsed.line, column: parsed.column } : null
}

const failureMessage = (errors, { format, errorText, wrapsThrownValues }) => {
  const [error, asyncError] = Array.isArray(errors) ? errors : [errors, new Error()]
  if (error && (typeof error.stack === 'string' || error.message)) return errorText(error)
  const message = `thrown: ${format(error)}`
  if (asyncError && !wrapsThrownValues) {
    asyncError.message = message
    return errorText(asyncError)
  }
  // Jest 30.5 wraps a non-Error throw in a new Error caused by the test's own
  // error. Jest creates that Error while building its results, so its stack
  // is Jest's internal frames; the copy here has none.
  const thrown = asyncError ? new Error(message, { cause: asyncError }) : new Error(message)
  thrown.stack = `Error: ${message}`
  return errorText(thrown)
}

// `status` is undefined while the test is still running. V8 formats an
// error's stack on first access using the message at that moment, and jest
// rewrites asyncError.message for non-Error throws before it reads the
// location, so the location is only read once the test has settled.
const assertionResult = (test, status, { includeLocation, formatting }) => {
  const names = []
  for (let block = test; block; block = block.parent) names.unshift(block.name)
  // jest-circus filters the root marker out of the whole path before it
  // pops the title, so a test titled ROOT_DESCRIBE_BLOCK keeps its title but
  // loses its describe blocks; the JSON upload has the same identity.
  const ancestorTitles = names.filter((name) => name !== ROOT_DESCRIBE_BLOCK_NAME)
  ancestorTitles.pop()
  const title = names[names.length - 1]

  const failureMessages = status === 'failed' ? test.errors.map((errors) => failureMessage(errors, formatting)) : []
  const location = includeLocation && status ? testLocation(test) : null

  return { ancestorTitles, title, location, status, failureMessages }
}

const BaseEnvironment = (() => {
  const mod = tryRequireFrom('jest-environment-node', [process.cwd(), __dirname])
  if (!mod) throw new Error("buildkite-test-collector/jest/environment requires 'jest-environment-node' to be installed")
  return mod.default || mod.TestEnvironment || mod
})()

// Adds Buildkite Test Engine OTel export to any Jest environment class. Each
// worker process owns one export session; this environment starts a
// test.execution span when a test starts, finishes it with the result jest
// settled on, and flushes when the test file is torn down (jest-worker exits
// the process without any later hook).
const extend = (Base) => class BuildkiteTestEngineEnvironment extends Base {
  static buildkiteTestEngine = true

  // Jest 28 passes { globalConfig, projectConfig }; Jest 27 passes the
  // project config alone. Nothing after super() may stop the tests from
  // running: without a session the file is simply not recorded as exported,
  // and the reporter uploads its results as JSON.
  constructor(config, context) {
    super(config, context)
    const projectConfig = config.projectConfig || config
    this._buildkite = { session: null, started: false, spans: new Map(), includeLocation: false, formatting: null, projectConfig, testPath: context?.testPath }
    let fallback = 'its results go to the JSON upload instead'
    try {
      const settings = readSettings()
      if (!settings) return
      fallback = settings.fallback
      const paths = new Paths({ rootDir: settings.rootDir }, settings.runEnv.location_prefix)
      this._buildkite.testPath = paths.prefixTestPath(context.testPath)
      this._buildkite.record = () => recordExport(settings.exportRecords, projectConfig, context.testPath)
      this._buildkite.fallback = fallback
      this._buildkite.session = sessionFor(settings)
    } catch (error) {
      warn(`OpenTelemetry could not be set up in this test worker (${describeError(error)}); for ${this._buildkite.testPath}, ${fallback}.`)
    }
  }

  async handleTestEvent(event, state) {
    if (this._buildkite.session && !this._buildkite.started) this._start(new Error().stack)
    if (super.handleTestEvent) await super.handleTestEvent(event, state)
    const { session, spans } = this._buildkite
    if (!session) return

    switch (event.name) {
      case 'include_test_location_in_result':
        this._buildkite.includeLocation = true
        break
      case 'test_start': {
        const result = this._assertionResult(event.test, undefined)
        const record = { id: randomUUID(), ...testEngineRecord(result, this._buildkite.testPath) }
        const handle = session.startTestSpan(record, { startTime: Date.now() })
        spans.set(event.test, { handle, id: record.id })
        break
      }
      case 'test_skip':
        this._finish(event.test, 'pending')
        break
      case 'test_todo':
        this._finish(event.test, 'todo')
        break
      case 'test_done':
        this._finish(event.test, event.test.errors.length ? 'failed' : 'passed')
        break
    }
  }

  // Runs on the first event, before any span: records the file as exported so
  // the reporter skips its JSON, and picks the failure formatting of the
  // jest-circus on the stack that dispatched the event.
  _start(stack) {
    this._buildkite.started = true
    try {
      this._buildkite.record()
    } catch (error) {
      warn(`OpenTelemetry could not record ${this._buildkite.testPath} as exported (${describeError(error)}); ${this._buildkite.fallback}.`)
      this._buildkite.session = null
      return
    }
    this._buildkite.formatting = failureFormatting(this._buildkite.projectConfig, dispatchingJestCircus(stack))
  }

  _assertionResult(test, status) {
    const { includeLocation, formatting } = this._buildkite
    return assertionResult(test, status, { includeLocation, formatting })
  }

  _finish(test, status) {
    const endTime = Date.now()
    const { session, spans, testPath } = this._buildkite
    const started = spans.get(test)
    spans.delete(test)
    if (!started) return
    let record = { id: started.id }
    let line
    try {
      const result = this._assertionResult(test, status)
      record = { ...record, ...testEngineRecord(result, testPath) }
      line = result.location?.line
    } catch (error) {
      warn(`Could not describe the finished test for OpenTelemetry: ${describeError(error)}`)
    }
    session.finishTestSpan(started.handle, record, { line, endTime })
  }

  async teardown() {
    try {
      const { session } = this._buildkite
      if (session) await session.forceFlush()
    } finally {
      await super.teardown()
    }
  }
}

module.exports = extend(BaseEnvironment)
module.exports.extend = extend
