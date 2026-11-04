const { randomBytes } = require('crypto')
const { version } = require('../../package.json')
const { loadSdk, assertCanExport } = require('./sdk')
const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { resourceAttributes, runAttributes } = require('./attributes')
const { describeTest, truncate } = require('./describeTest')
const { warn, describeError, PREFIX } = require('./warn')
const { createChildExport } = require('./children')

const TRACER_NAME = 'buildkite-test-collector'
const TEST_SPAN_NAME = 'test.execution'
const SPAN_STREAM = 'test'
const FLUSH_BUDGET_MILLISECONDS = 30000
const TIMED_OUT = Symbol('timed out')

// Match server code-point limits before the SDK applies its UTF-16 limit.
const SPAN_ATTRIBUTE_VALUE_MAX_LENGTH = 10243
const SPAN_LIMITS = {
  attributeCountLimit: 128,
  attributeValueLengthLimit: 2 * 16384,
  eventCountLimit: 100,
  attributePerEventCountLimit: 128,
  linkCountLimit: 128,
  attributePerLinkCountLimit: 128,
}

const limitAttributes = (attributes) => Object.fromEntries(Object.entries(attributes).map(([key, value]) => [
  key, typeof value === 'string' ? truncate(value, SPAN_ATTRIBUTE_VALUE_MAX_LENGTH) : value,
]))

const PROCESSOR_DEFAULTS = { queue: 8192, batch: 120 }
const PROCESSOR_SIZE_LIMIT = 2147483647
const SCHEDULE_DELAY_MILLISECONDS = 1000

const processorSize = (name, fallback, limit = PROCESSOR_SIZE_LIMIT) => {
  const raw = process.env[name]
  if (raw === undefined) return fallback
  const value = /^[0-9]+$/.test(raw) ? Number(raw) : NaN
  if (value >= 1 && value <= limit) return value
  warn(`${name} must be a positive integer up to ${limit}, using default ${fallback}`)
  return fallback
}

const processorSizes = (stream = 'TEST', defaults = PROCESSOR_DEFAULTS) => {
  const queueName = `BUILDKITE_TESTS_OTEL_${stream}_SPAN_QUEUE_SIZE`
  const batchName = `BUILDKITE_TESTS_OTEL_${stream}_SPAN_BATCH_SIZE`
  const queue = processorSize(queueName, defaults.queue)
  const batch = processorSize(batchName, defaults.batch, stream === 'TEST' ? 200 : PROCESSOR_SIZE_LIMIT)
  if (batch > queue) {
    warn(`${batchName} must be <= ${queueName}; using defaults (batch ${defaults.batch}, queue ${defaults.queue})`)
    return defaults
  }
  return { queue, batch }
}

// Test suites stub Math.random, which the SDK's default generator uses;
// all-zero ids are invalid, hence the retry.
const randomHex = (bytes) => {
  for (;;) {
    const hex = randomBytes(bytes).toString('hex')
    if (/[^0]/.test(hex)) return hex
  }
}
const idGenerator = {
  generateTraceId: () => randomHex(16),
  generateSpanId: () => randomHex(8),
}

// Link to the Agent job without making otherwise independent tests share a trace.
const jobSpanLinks = ({ api, core }) => {
  const context = core.parseTraceParent((process.env.TRACEPARENT || '').trim())
  if (!context) return []
  context.traceState = process.env.TRACESTATE ? api.createTraceState(process.env.TRACESTATE) : undefined
  context.isRemote = true
  return [{ context }]
}

// Keep both lines together in interleaved worker output.
const missingResults = (count, detail) => {
  warn(
    `TEST RESULTS MISSING: OpenTelemetry dropped ${count} test.execution span(s) (${detail}).\n` +
    `${PREFIX} OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.`
  )
}

const pendingResults = (count) => {
  warn(
    `TEST RESULTS MISSING: ${count} test.execution span(s) were still exporting when the collector stopped waiting for them ` +
    'and may not have reached Buildkite Test Engine.'
  )
}

// Explicit settings keep suite OTEL_* settings from sampling or redirecting results.
const createSession = (settings) => {
  const sdk = loadSdk()
  const { api, trace, resources } = sdk
  const { runEnv } = settings
  const runKey = runEnv.key
  const providerConfig = {
    sampler: new trace.AlwaysOnSampler(),
    idGenerator,
    resource: resources.defaultResource()
      .merge(resources.detectResources({ detectors: [resources.envDetector] }))
      .merge(resources.resourceFromAttributes(resourceAttributes(runEnv))),
    spanLimits: SPAN_LIMITS,
  }
  assertCanExport(sdk, providerConfig)

  const metrics = {
    started: 0,
    startFailures: 0,
    ended: 0,
    handed: 0,
    exported: 0,
    failed: 0,
    lastFailure: null,
    flushBudget: FLUSH_BUDGET_MILLISECONDS,
  }
  let reportedFailures = 0

  // Report each failed or pending span once, including before Jest force-exits.
  const unsettled = new Set()
  let unreportedFailures = 0
  const settle = (spans, succeeded) => {
    for (const span of spans) {
      if (unsettled.delete(span) && !succeeded) unreportedFailures += 1
    }
    feedHeldSpans()
  }

  // Hold overflow in memory: the SDK would drop results from a full queue.
  const sizes = processorSizes()
  const held = []
  let queued = 0
  const feedHeldSpans = () => {
    let fed = 0
    while (held.length > 0 && queued - metrics.handed < sizes.queue) {
      queued += 1
      fed += 1
      processor.onEnd(held.shift())
    }
    return fed
  }
  const exporter = createExporter({
    sdk,
    endpoint: settings.endpoint,
    token: settings.token,
    runKey,
    stream: SPAN_STREAM,
    metrics,
    onSettled: settle,
  })
  const processor = new trace.BatchSpanProcessor(exporter, {
    maxQueueSize: sizes.queue,
    maxExportBatchSize: sizes.batch,
    scheduledDelayMillis: SCHEDULE_DELAY_MILLISECONDS,
    exportTimeoutMillis: EXPORT_TIMEOUT_MILLISECONDS,
  })
  const holdingProcessor = {
    onStart: (span, context) => processor.onStart(span, context),
    onEnd: (span) => {
      unsettled.add(span)
      held.push(span)
      feedHeldSpans()
    },
    forceFlush: () => processor.forceFlush(),
    shutdown: () => processor.shutdown(),
  }
  const provider = new trace.BasicTracerProvider({ ...providerConfig, spanProcessors: [holdingProcessor] })
  const tracer = provider.getTracer(TRACER_NAME, version)
  const run = runAttributes(settings)
  let children
  const childExport = () => children ||= createChildExport({ sdk, settings, processorSizes })
  // Unlike test results, children honor the suite's sampler and span limits.
  const childProviderConfig = { resource: providerConfig.resource, idGenerator }
  let propagator
  const childPropagator = () => propagator ||= new sdk.core.CompositePropagator({
    propagators: [new sdk.core.W3CTraceContextPropagator(), new sdk.core.W3CBaggagePropagator()],
  })

  const startTestSpan = (record, { line, startTime } = {}) => {
    try {
      const { attributes } = describeTest(record, { line })
      const span = tracer.startSpan(TEST_SPAN_NAME, {
        root: true,
        kind: api.SpanKind.INTERNAL,
        startTime,
        links: jobSpanLinks(sdk),
        attributes: limitAttributes({
          'buildkite.execution.via': 'otlp',
          'buildkite.run_key': runKey,
          'test.case.result.status': 'unset',
          ...attributes,
        }),
      }, api.ROOT_CONTEXT)
      metrics.started += 1
      return { span, startTime: startTime ?? Date.now() }
    } catch (error) {
      metrics.startFailures += 1
      missingResults(1, `could not start span: ${describeError(error)}`)
      return null
    }
  }

  const recordResult = (span, description) => {
    if (!description.status) return
    span.setAttribute('test.case.result.status', description.status)
    if (description.status !== 'fail') return
    span.setStatus({ code: api.SpanStatusCode.ERROR, message: description.failureReason })
    for (const event of description.exceptionEvents) {
      span.addEvent(event.name, event.attributes)
    }
  }

  const finishTestSpan = (handle, record, { line, endTime } = {}) => {
    if (!handle) return
    const { span } = handle
    let description
    try {
      description = describeTest(record, { line })
      recordResult(span, description)
    } catch (error) {
      warn(`Could not record the OpenTelemetry test result: ${describeError(error)}`)
    }
    try {
      if (description) {
        span.setAttributes(limitAttributes({ ...description.attributes, ...run }))
      }
    } catch (error) {
      warn(`Could not describe OpenTelemetry test span: ${describeError(error)}`)
    }
    try {
      if (endTime !== undefined && endTime >= handle.startTime) {
        span.end(endTime)
      } else {
        span.end()
      }
      metrics.ended += 1
    } catch (error) {
      warn(`Could not finish OpenTelemetry test span: ${describeError(error)}`)
    }
  }

  // Each stream spends its own budget only on failures/timeouts. The exporter's
  // flush can resolve despite failed batches, so also check its failure count.
  const awaitWithinBudget = async (promise, account = metrics) => {
    if (account.flushBudget <= 0) {
      promise.catch(() => {})
      return false
    }
    const startedAt = Date.now()
    const failedBefore = account.failed
    let rejected = false
    let timer
    const expiry = new Promise((resolve) => { timer = setTimeout(resolve, account.flushBudget, TIMED_OUT) })
    try {
      const outcome = await Promise.race([promise, expiry])
      if (outcome === TIMED_OUT) {
        account.flushBudget = 0
        return false
      }
      return true
    } catch (error) {
      rejected = true
      throw error
    } finally {
      clearTimeout(timer)
      if (rejected || account.failed > failedBefore) {
        account.flushBudget = Math.max(0, account.flushBudget - (Date.now() - startedAt))
      }
    }
  }

  // Only a completed drain proves an unsettled span never reached the exporter.
  const reportUndelivered = ({ drained }) => {
    reportedFailures = metrics.failed
    const leftover = unsettled.size
    unsettled.clear()
    const notExported = drained ? leftover : 0
    const reasons = []
    if (notExported > 0) reasons.push(`not-exported: ${notExported}`)
    if (unreportedFailures > 0) {
      const cause = metrics.lastFailure ? `, last OTLP failure: ${metrics.lastFailure}` : ''
      reasons.push(`export-failure: ${unreportedFailures}${cause}`)
    }
    if (reasons.length > 0) missingResults(notExported + unreportedFailures, reasons.join(', '))
    unreportedFailures = 0
    if (!drained && leftover > 0) pendingResults(leftover)
  }

  // Join in-flight exports as well as queued batches, including spans fed during a flush.
  const drain = async () => {
    let failure = null
    for (;;) {
      feedHeldSpans()
      const queuedBefore = queued
      try {
        if (!await awaitWithinBudget(processor.forceFlush().finally(() => exporter.forceFlush()))) return { drained: false, failure }
      } catch (error) {
        failure = error
      }
      if (held.length === 0 && queued === queuedBefore) return { drained: true, failure }
      // Nothing fitting an emptied queue means the processor has shut down.
      if (held.length > 0 && feedHeldSpans() === 0) return { drained: false, failure }
    }
  }

  const warnUnlessCounted = (action, failure) => {
    const cause = failure && metrics.failed === reportedFailures && describeError(failure)
    if (cause) warn(`Could not ${action}: ${cause}`)
  }

  const forceFlush = async () => {
    const childFlush = children && awaitWithinBudget(children.flush(), children.metrics).catch(() => true)
    const { drained, failure } = await drain()
    warnUnlessCounted('flush OpenTelemetry spans', failure)
    reportUndelivered({ drained })
    // Rejected child flushes have already counted the failed batch.
    if (children) children.report(await childFlush)
  }

  const onExit = () => {
    reportUndelivered({ drained: false })
    if (children) children.report(false, { final: true })
  }
  process.on('exit', onExit)

  let shutDown = false
  const shutdown = async () => {
    if (shutDown) return
    shutDown = true
    let { drained, failure } = await drain()
    warnUnlessCounted('flush OpenTelemetry spans', failure)
    try {
      drained = await awaitWithinBudget(provider.shutdown()) && drained
    } catch (error) {
      warnUnlessCounted('shut down OpenTelemetry span export', error)
    }
    reportUndelivered({ drained })
    if (drained) process.off('exit', onExit)
  }

  return { api, runKey, metrics, startTestSpan, finishTestSpan, forceFlush, shutdown, childExport, childProviderConfig, childPropagator }
}

let current = null

const sessionFor = (settings) => {
  if (current) {
    if (settings.runEnv.key !== current.runKey) {
      warn(
        `OpenTelemetry span export is already configured for run ${JSON.stringify(current.runKey)} and cannot switch to run ` +
        `${JSON.stringify(settings.runEnv.key)}: results will still be attributed to the earlier run. Reporting a new run requires a new process.`
      )
    }
    return current
  }
  const session = createSession(settings)
  const shared = {
    ...session,
    shutdown: async () => {
      await session.shutdown()
      if (current === shared) current = null
    },
  }
  current = shared
  return shared
}

module.exports = { createSession, sessionFor }
