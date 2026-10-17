const { randomUUID } = require('crypto')
const Paths = require('../util/paths')
const { readSettings } = require('../util/otel/config')
const { sessionFor } = require('../util/otel/session')
const { warn, describeError } = require('../util/otel/warn')
const testEngineRecord = require('./testEngineRecord')

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

const prettyFormatter = (rootDir) => {
  const prettyFormat = tryRequireFrom('pretty-format', [rootDir, __dirname])
  return prettyFormat ? (value) => prettyFormat.format(value, { maxDepth: 3 }) : String
}

// parseStackLine through assertionResult mirror how jest-circus turns a
// finished test into the assertion result the reporter sees, so the span
// carries the same title, ancestors, location and failure text.
const parseStackLine = (line) => {
  const match = STACK_LINE.exec(line || '')
  return match ? { file: match[1], line: Number(match[2]), column: Number(match[3]) } : null
}

const testLocation = (test) => {
  const stackLines = test.asyncError.stack.split('\n')
  let parsed = parseStackLine(stackLines[1])
  if (parsed && JEST_EACH_BUILD_DIR.test(parsed.file)) parsed = parseStackLine(stackLines[4])
  return parsed ? { line: parsed.line, column: parsed.column } : null
}

const failureMessage = (errors, format) => {
  const [error, asyncError] = Array.isArray(errors) ? errors : [errors, new Error()]
  if (error && (typeof error.stack === 'string' || error.message)) {
    return typeof error.stack === 'string' ? error.stack : error.message
  }
  asyncError.message = `thrown: ${format(error)}`
  return typeof asyncError.stack === 'string' ? asyncError.stack : asyncError.message
}

// `status` is undefined while the test is still running. V8 formats an
// error's stack on first access using the message at that moment, and jest
// rewrites asyncError.message for non-Error throws before it reads the
// location, so the location is only read once the test has settled.
const assertionResult = (test, status, { includeLocation, format }) => {
  const names = []
  for (let block = test; block; block = block.parent) names.unshift(block.name)
  const ancestorTitles = names.filter((name) => name !== ROOT_DESCRIBE_BLOCK_NAME)
  const title = ancestorTitles.pop()

  const failureMessages = status === 'failed' ? test.errors.map((errors) => failureMessage(errors, format)) : []
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

  constructor(config, context) {
    super(config, context)
    this._buildkite = {
      session: null,
      spans: new Map(),
      includeLocation: false,
      format: prettyFormatter(config.projectConfig.rootDir),
      testPath: context.testPath,
    }
    try {
      const settings = readSettings()
      if (!settings) return
      this._buildkite.testPath = new Paths(config.globalConfig, settings.runEnv.location_prefix).prefixTestPath(context.testPath)
      this._buildkite.session = sessionFor(settings)
    } catch (error) {
      warn(
        `TEST RESULTS MISSING: OpenTelemetry could not be set up in this test worker (${describeError(error)}); ` +
        `test executions from ${context.testPath} were not uploaded to Buildkite Test Engine.`
      )
    }
  }

  async handleTestEvent(event, state) {
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

  _assertionResult(test, status) {
    const { includeLocation, format } = this._buildkite
    return assertionResult(test, status, { includeLocation, format })
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
