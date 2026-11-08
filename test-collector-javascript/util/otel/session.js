const { version } = require('../../package.json')
const { loadSdk, createProvider, createResource, createPropagator, assertCanExport } = require('./sdk')
const { runAttributes } = require('./attributes')
const { describeTest, truncate } = require('./describeTest')
const { warn, describeError } = require('./warn')
const { createChildExport } = require('./children')
const { commitFile } = require('./spool')

const limitAttributes = (attributes) => Object.fromEntries(Object.entries(attributes).map(([key, value]) => [
  key, typeof value === 'string' ? truncate(value, 10243) : value,
]))

const jobSpanLinks = ({ api, core }) => {
  const context = core.parseTraceParent((process.env.TRACEPARENT || '').trim())
  if (!context) return []
  context.traceState = process.env.TRACESTATE ? api.createTraceState(process.env.TRACESTATE) : undefined
  context.isRemote = true
  return [{ context }]
}

// Workers retain only this file's spans. No timers, exporters or network I/O.
const createSession = (settings) => {
  const sdk = loadSdk()
  const { api } = sdk
  const runKey = settings.runEnv.key
  let primed
  const resource = createResource(sdk, settings.runEnv)
  const providerConfig = {
    testSpans: true, resource, getIds: () => primed,
  }
  assertCanExport(sdk, providerConfig)
  let spans = []
  const processor = {
    onStart() {}, onEnd(span) { spans.push(span) },
    forceFlush: async () => {}, shutdown: async () => {},
  }
  const provider = createProvider(sdk, { ...providerConfig, spanProcessors: [processor] })
  const tracer = provider.getTracer('buildkite-test-collector', version)
  const run = runAttributes(settings)
  let children
  const childExport = () => children ||= createChildExport(settings)
  const createChildProvider = (spanProcessors) => createProvider(sdk, { resource, spanProcessors })
  let propagator
  const childPropagator = () => propagator ||= createPropagator(sdk)

  const startTestSpan = (record, { line, column, startTime, ids } = {}) => {
    try {
      const { attributes } = describeTest(record, { line, column })
      primed = ids
      const span = tracer.startSpan('test.execution', {
        root: true, kind: api.SpanKind.INTERNAL, startTime, links: jobSpanLinks(sdk),
        attributes: limitAttributes({
          'buildkite.execution.via': 'otlp', 'buildkite.run_key': runKey,
          'test.case.result.status': 'unset', ...attributes,
        }),
      }, api.ROOT_CONTEXT)
      return { span, startTime: startTime ?? Date.now() }
    } catch (error) {
      warn(`TEST RESULTS MISSING: could not start 1 test.execution span (${describeError(error)}); results were not uploaded.`)
      return null
    } finally {
      primed = null
    }
  }

  const finishTestSpan = (handle, record, { line, column, endTime } = {}) => {
    if (!handle) return
    const { span } = handle
    try {
      const description = describeTest(record, { line, column })
      if (description.status) span.setAttribute('test.case.result.status', description.status)
      if (description.status === 'fail') {
        span.setStatus({ code: api.SpanStatusCode.ERROR, message: description.failureReason })
        for (const event of description.exceptionEvents) span.addEvent(event.name, event.attributes)
      }
      span.setAttributes(limitAttributes({ ...description.attributes, ...run }))
      for (const [key, value] of Object.entries(record.tags || {})) {
        if (value !== undefined && value !== null) span.setAttribute(`buildkite.tag.${key}`, truncate(String(value), 10243))
      }
    } catch (error) {
      warn(`Could not describe OpenTelemetry test span: ${describeError(error)}`)
    }
    span.end(endTime !== undefined && endTime >= handle.startTime ? endTime : undefined)
  }

  const forceFlush = async (testPath, projectId) => {
    const tests = spans
    spans = []
    const childSpans = children?.take() || []
    try {
      commitFile(settings.spool, testPath, { test: tests, child: childSpans }, projectId)
    } catch (error) {
      warn(`TEST RESULTS MISSING: ${tests.length} test.execution span(s), ${childSpans.length} child span(s) were not uploaded: could not commit the local spool (${describeError(error)}). Check the temporary directory's permissions and free space, then rerun.`)
    }
  }

  return {
    api, runKey, startTestSpan, finishTestSpan, forceFlush,
    shutdown: () => provider.shutdown(), childExport, createChildProvider, childPropagator,
  }
}

let current = null
const sessionFor = (settings) => {
  if (current) {
    current.settings.spool = settings.spool
    if (settings.runEnv.key !== current.session.runKey) warn(`OpenTelemetry span export is already configured for run ${JSON.stringify(current.session.runKey)} and cannot switch to run ${JSON.stringify(settings.runEnv.key)}: use a new process.`)
    return current.session
  }
  const session = createSession(settings)
  const shutdown = session.shutdown
  session.shutdown = async () => {
    await shutdown()
    if (current?.session === session) current = null
  }
  current = { settings, session }
  return session
}

module.exports = { createSession, sessionFor }
