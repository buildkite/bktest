const { randomBytes } = require('crypto')
const { version } = require('../../package.json')
const { loadSdk, assertCanExport } = require('./sdk')
const { resourceAttributes, runAttributes } = require('./attributes')
const { describeTest, truncate } = require('./describeTest')
const { warn, describeError } = require('./warn')
const { createChildExport } = require('./children')
const { commitFile } = require('./spool')

const SPAN_LIMITS = {
  attributeCountLimit: 128, attributeValueLengthLimit: 2 * 16384,
  eventCountLimit: 100, attributePerEventCountLimit: 128,
  linkCountLimit: 128, attributePerLinkCountLimit: 128,
}
const limitAttributes = (attributes) => Object.fromEntries(Object.entries(attributes).map(([key, value]) => [
  key, typeof value === 'string' ? truncate(value, 10243) : value,
]))

// Suites can stub Math.random. Never let that produce invalid all-zero IDs.
const randomHex = (bytes) => {
  for (;;) {
    const hex = randomBytes(bytes).toString('hex')
    if (/[^0]/.test(hex)) return hex
  }
}
const idGenerator = { generateTraceId: () => randomHex(16), generateSpanId: () => randomHex(8) }

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
  const { api, trace, resources } = sdk
  const runKey = settings.runEnv.key
  const providerConfig = {
    sampler: new trace.AlwaysOnSampler(), idGenerator,
    resource: resources.defaultResource()
      .merge(resources.detectResources({ detectors: [resources.envDetector] }))
      .merge(resources.resourceFromAttributes(resourceAttributes(settings.runEnv))),
    spanLimits: SPAN_LIMITS,
  }
  assertCanExport(sdk, providerConfig)
  let spans = []
  const processor = {
    onStart() {}, onEnd(span) { spans.push(span) },
    forceFlush: async () => {}, shutdown: async () => {},
  }
  const provider = new trace.BasicTracerProvider({ ...providerConfig, spanProcessors: [processor] })
  const tracer = provider.getTracer('buildkite-test-collector', version)
  const run = runAttributes(settings)
  let children
  const childExport = () => children ||= createChildExport(settings)
  const childProviderConfig = { resource: providerConfig.resource, idGenerator }
  let propagator
  const childPropagator = () => propagator ||= new sdk.core.CompositePropagator({
    propagators: [new sdk.core.W3CTraceContextPropagator(), new sdk.core.W3CBaggagePropagator()],
  })

  const startTestSpan = (record, { line, column, startTime } = {}) => {
    try {
      const { attributes } = describeTest(record, { line, column })
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
        if (value !== undefined && value !== null) span.setAttribute(`buildkite.tag.${key}`, String(value))
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
    shutdown: () => provider.shutdown(), childExport, childProviderConfig, childPropagator,
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
