const { randomUUID } = require('crypto')
const path = require('path')
const { types } = require('util')
const Paths = require('../util/paths')
const { readSettings } = require('../util/otel/config')
const { sessionFor } = require('../util/otel/session')
const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')
const testEngineRecord = require('./testEngineRecord')
const JestOtelContext = require('./otelContext')

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

// Use the running Circus version's failure text so JSON and spans agree.
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

// A custom runner may use a different Circus than the one installed beside it.
const JEST_CIRCUS_BUILD_FILE = /^(.*[\\/]jest-circus)[\\/]build[\\/]/
const dispatchingJestCircus = (stack) => {
  for (const line of stack.split('\n').slice(1)) {
    const match = JEST_CIRCUS_BUILD_FILE.exec(parseStackLine(line)?.file || '')
    if (match) return match[1]
  }
  return null
}

const parseStackLine = (line) => {
  const match = STACK_LINE.exec(line || '')
  return match ? { file: match[1], line: Number(match[2]), column: Number(match[3]) } : null
}

// each() adds internal frames; skipping them finds the user frame on Jest 29 and 30.
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
  // Jest 30.5's wrapper stack contains only internal frames; omit those here.
  const thrown = asyncError ? new Error(message, { cause: asyncError }) : new Error(message)
  thrown.stack = `Error: ${message}`
  return errorText(thrown)
}

// Defer reading the stack until Circus has rewritten non-Error throws.
const assertionResult = (test, status, { includeLocation, formatting }) => {
  const names = []
  for (let block = test; block; block = block.parent) names.unshift(block.name)
  // Match Circus even when a test itself is named ROOT_DESCRIBE_BLOCK.
  const ancestorTitles = names.filter((name) => name !== ROOT_DESCRIBE_BLOCK_NAME)
  ancestorTitles.pop()
  const title = names.at(-1)

  const failureMessages = status === 'failed' ? test.errors.map((errors) => failureMessage(errors, formatting)) : []
  const location = includeLocation && status ? testLocation(test) : null

  return { ancestorTitles, title, location, status, failureMessages }
}

class JestOtelEnvironment {
  // Setup failures leave tests running, but never switch the upload format.
  constructor(config, context) {
    const { globalConfig, projectConfig } = config
    this._buildkite = { session: null, started: false, spans: new Map(), includeLocation: false, formatting: null, projectConfig, testPath: context?.testPath }
    let settings
    try {
      settings = readSettings()
      if (!settings) return
      const paths = new Paths({ rootDir: globalConfig?.rootDir ?? settings.rootDir }, settings.runEnv.location_prefix)
      this._buildkite.testPath = paths.prefixTestPath(context.testPath)
      this._buildkite.sourcePath = context.testPath
      this._buildkite.session = sessionFor(settings)
      this._buildkite.children = new JestOtelContext({
        session: this._buildkite.session, settings, testPath: context.testPath, rootDir: projectConfig.rootDir,
      })
    } catch (error) {
      this._buildkite.session = null
      warnOncePerRun(settings?.spool, 'worker-setup', `TEST RESULTS MISSING: OpenTelemetry could not be set up in this test worker (${describeError(error)}); results for ${this._buildkite.testPath} were not uploaded. Fix the SDK/runtime configuration and rerun (no JSON fallback).`)
    }
  }

  async handleTestEvent(event, state, handleTestEvent) {
    if (this._buildkite.session && !this._buildkite.started) this._start(new Error().stack)
    if (handleTestEvent) await handleTestEvent(event, state)
    const { session, spans, children } = this._buildkite
    if (!session) return

    switch (event.name) {
      case 'run_start':
        this._wrapFunctions(state.rootDescribeBlock)
        break
      case 'include_test_location_in_result':
        this._buildkite.includeLocation = true
        break
      case 'test_start': {
        // Circus starts test.concurrent bodies before their test_start, and
        // their wrappers already started the test's span; only a previous
        // attempt's span is finished here.
        if (spans.get(event.test)?.endTime !== undefined) this._finish(event.test)
        this._begin(event.test)
        break
      }
      case 'hook_start': {
        this._buildkite.hookTest = state.currentlyRunningTest
        break
      }
      case 'test_skip':
        this._finish(event.test, 'pending')
        break
      case 'test_todo':
        this._finish(event.test, 'todo')
        break
      case 'test_done': {
        const started = spans.get(event.test)
        if (started) {
          started.endTime = Date.now()
          // Circus replaces this array when retrying a test or describe.
          started.errors = event.test.errors
        }
        break
      }
    }
  }

  _begin(test) {
    const { session, spans, children, testPath } = this._buildkite
    if (spans.has(test)) return spans.get(test)
    const result = assertionResult(test, undefined, this._buildkite)
    const record = { id: randomUUID(), ...testEngineRecord(result, testPath) }
    const handle = session.startTestSpan(record, { startTime: Date.now() })
    const started = { handle, id: record.id }
    spans.set(test, started)
    children.began(started)
    return started
  }

  _wrapFunctions(block) {
    const { children, spans } = this._buildkite
    const wrap = (entry, test) => {
      const fn = entry.fn
      if (!fn) return
      const invoke = (receiver, args) => {
        const owner = test || this._buildkite.hookTest
        if (!owner || (!test && !['beforeEach', 'afterEach'].includes(entry.type))) return fn.apply(receiver, args)
        const started = test ? this._begin(owner) : spans.get(owner)
        return children.run(started, fn, receiver, args)
      }
      // Circus uses .length to distinguish callback tests/hooks. Do not
      // replace a done-style function with a zero-arity rest wrapper.
      entry.fn = fn.length
        ? function (done) { return invoke(this, [done]) }
        : function () { return invoke(this, []) }
      // Circus recognises generator tests by their function prototype.
      Object.setPrototypeOf(entry.fn, Object.getPrototypeOf(fn))
    }
    for (const hook of block.hooks) wrap(hook)
    for (const child of block.children) {
      if (child.type === 'describeBlock') this._wrapFunctions(child)
      else wrap(child, child)
    }
  }

  _start(stack) {
    this._buildkite.started = true
    this._buildkite.formatting = failureFormatting(this._buildkite.projectConfig, dispatchingJestCircus(stack))
  }

  _finish(test, status) {
    const { session, spans, testPath } = this._buildkite
    const started = spans.get(test)
    spans.delete(test)
    if (!started) return
    const endTime = started.endTime ?? Date.now()
    const errors = started.errors || test.errors
    let record = { id: started.id }
    let line
    try {
      const result = assertionResult({ ...test, errors }, status || (errors.length ? 'failed' : started.endTime !== undefined ? 'passed' : undefined), this._buildkite)
      record = { ...record, ...testEngineRecord(result, testPath) }
      line = result.location?.line
    } catch (error) {
      warn(`Could not describe the finished test for OpenTelemetry: ${describeError(error)}`)
    }
    session.finishTestSpan(started.handle, record, { line, endTime })
  }

  async teardown() {
    const { session, spans, started } = this._buildkite
    if (session && started) {
      // Late test-owned rejections can arrive during afterAll.
      for (const test of spans.keys()) this._finish(test)
      try {
        await this._buildkite.children.teardown()
      } catch (error) {
        warn(`Could not shut down OpenTelemetry child instrumentation: ${describeError(error)}`)
      } finally {
        await session.forceFlush(this._buildkite.sourcePath)
      }
    }
  }
}

const attach = (environment, config, context) => {
  const collector = new JestOtelEnvironment(config, context)
  if (!collector._buildkite.session) return
  environment._buildkite = collector._buildkite
  const setup = environment.setup.bind(environment)
  environment.setup = async () => {
    await setup()
    // Custom environments may install their handler during setup.
    const handleTestEvent = environment.handleTestEvent?.bind(environment)
    environment.handleTestEvent = (event, state) => collector.handleTestEvent(event, state, handleTestEvent)
    const teardown = environment.teardown.bind(environment)
    environment.teardown = async () => {
      try { await collector.teardown() } finally { await teardown() }
    }
  }
}

module.exports = { attach, JestOtelEnvironment }
