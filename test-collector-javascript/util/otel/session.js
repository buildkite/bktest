const { randomBytes } = require('crypto')
const { version } = require('../../package.json')
const { loadSdk, assertCanExport } = require('./sdk')
const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { resourceAttributes, runAttributes } = require('./attributes')
const { describeTest, truncate } = require('./describeTest')
const { warn, describeError, PREFIX } = require('./warn')

const TRACER_NAME = 'buildkite-test-collector'
const TEST_SPAN_NAME = 'test.execution'
const SPAN_STREAM = 'test'
const FLUSH_BUDGET_MILLISECONDS = 30000
const TIMED_OUT = Symbol('timed out')

// The span's own string attributes are cut to 10,243 code points here: the
// server truncates test names to 10,240 before deriving the test ID, so a
// long name keeps the identity of its JSON upload. The SDK applies one limit,
// in UTF-16 code units, to span and event attributes alike, so it is set to
// the widest encoding of the event attribute limit the other collectors use
// (16,384 code points) and only ever cuts a value the collector missed.
const SPAN_ATTRIBUTE_VALUE_MAX_LENGTH = 10243
const SPAN_LIMITS = {
  attributeCountLimit: 128,
  attributeValueLengthLimit: 2 * 16384,
  eventCountLimit: 100,
  attributePerEventCountLimit: 128,
}

const limitAttributes = (attributes) => {
  const limited = {}
  for (const [key, value] of Object.entries(attributes)) {
    limited[key] = typeof value === 'string' ? truncate(value, SPAN_ATTRIBUTE_VALUE_MAX_LENGTH) : value
  }
  return limited
}

const PROCESSOR_DEFAULTS = { queue: 8192, batch: 120 }
const PROCESSOR_SIZE_LIMIT = 2147483647
const SCHEDULE_DELAY_MILLISECONDS = 1000

const processorSize = (name, fallback) => {
  const raw = process.env[name]
  if (raw === undefined) return fallback
  const value = /^[0-9]+$/.test(raw) ? Number(raw) : NaN
  if (value >= 1 && value <= PROCESSOR_SIZE_LIMIT) return value
  warn(`${name} must be a positive integer up to ${PROCESSOR_SIZE_LIMIT}, using default ${fallback}`)
  return fallback
}

const processorSizes = () => {
  const queueName = 'BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE'
  const batchName = 'BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE'
  const queue = processorSize(queueName, PROCESSOR_DEFAULTS.queue)
  const batch = processorSize(batchName, PROCESSOR_DEFAULTS.batch)
  if (batch > queue) {
    warn(`${batchName} must be <= ${queueName}; using defaults (batch ${PROCESSOR_DEFAULTS.batch}, queue ${PROCESSOR_DEFAULTS.queue})`)
    return PROCESSOR_DEFAULTS
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

// W3C traceparent: version 00 has exactly four fields; later versions may
// append more, version ff is reserved.
const TRACEPARENT = /^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-.*)?$/

// The Agent exports its job span's context; a link (not a parent) keeps the
// test span a trace root while still connecting the two.
const jobSpanLinks = (api) => {
  const match = TRACEPARENT.exec((process.env.TRACEPARENT || '').trim())
  if (!match || match[1] === 'ff' || (match[1] === '00' && match[5])) return []
  const [, , traceId, spanId, flags] = match
  const traceState = process.env.TRACESTATE ? api.createTraceState(process.env.TRACESTATE) : undefined
  const context = { traceId, spanId, traceFlags: parseInt(flags, 16), traceState, isRemote: true }
  return api.isSpanContextValid(context) ? [{ context }] : []
}

// One console.warn call keeps both lines together in interleaved worker
// output; each carries the prefix so either line can be grepped.
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

// Owns the private tracer provider for one run in this process: every test
// span is a trace root exported on the `test` stream with explicit settings,
// so customer OTEL_* configuration cannot sample, trim or redirect results.
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

  // Every ended span stays here until its export settles or a warning has
  // counted it, so each span is reported once at most: as an export failure,
  // as never exported, or as pending when a flush stops waiting for it. A
  // worker that Jest force-exits has then already warned about every span it
  // could not deliver.
  const unsettled = new Set()
  let unreportedFailures = 0
  const settle = (spans, succeeded) => {
    for (const span of spans) {
      if (unsettled.delete(span) && !succeeded) unreportedFailures += 1
    }
    feedHeldSpans()
  }

  // The batch processor drops a span when its queue is full, and a run of
  // fast tests ends spans faster than one Node thread can export them, so
  // spans that would overflow the queue are held here instead and handed
  // over as exports complete. Memory is the only cap. The queue's length is
  // tracked as spans handed to the processor minus spans it has handed on,
  // which the processor does synchronously as it starts each export.
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

  const startTestSpan = (record, { line, startTime } = {}) => {
    try {
      const { attributes } = describeTest(record, { line })
      const span = tracer.startSpan(TEST_SPAN_NAME, {
        root: true,
        kind: api.SpanKind.INTERNAL,
        startTime,
        links: jobSpanLinks(api),
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
        for (const [key, value] of Object.entries(limitAttributes({ ...description.attributes, ...run }))) {
          span.setAttribute(key, value)
        }
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

  // Flush and shutdown share one 30 s budget per process: waits that end in
  // failure or run out of time consume it, and once it is gone later flushes
  // stop waiting (export continues in the background). A healthy endpoint
  // never consumes it, so a long run of test files keeps waiting for each one.
  //
  // Failure is judged by the exporter's count, not by the promise: a batch
  // the processor timer handed over before the flush fails inside
  // exporter.forceFlush(), which resolves, so a rejected promise alone would
  // let an outage charge every test file the full retry delay.
  const awaitWithinBudget = async (promise) => {
    if (metrics.flushBudget <= 0) {
      promise.catch(() => {})
      return false
    }
    const startedAt = Date.now()
    const failedBefore = metrics.failed
    let rejected = false
    let timer
    const expiry = new Promise((resolve) => { timer = setTimeout(resolve, metrics.flushBudget, TIMED_OUT) })
    try {
      const outcome = await Promise.race([promise, expiry])
      if (outcome === TIMED_OUT) {
        metrics.flushBudget = 0
        return false
      }
      return true
    } catch (error) {
      rejected = true
      throw error
    } finally {
      clearTimeout(timer)
      if (rejected || metrics.failed > failedBefore) {
        metrics.flushBudget = Math.max(0, metrics.flushBudget - (Date.now() - startedAt))
      }
    }
  }

  // After a completed drain, a span that never settled never reached the
  // exporter; otherwise it may still be exporting and is reported as pending.
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

  // The processor rejects a flush when any batch in it failed, which the
  // exporter wrapper has already counted; only an unexpected failure is worth
  // its own warning.
  const unexplained = (error) => metrics.failed === reportedFailures && describeError(error)

  // The processor's flush only covers its queue; a batch its timer already
  // handed to the exporter (retries included) is joined through the exporter,
  // and spans held back from a full queue are fed in and flushed again until
  // none are left or the budget runs out. A failed batch has been counted by
  // the exporter and does not stop the rest from being sent.
  const drain = async () => {
    let failure = null
    for (;;) {
      feedHeldSpans()
      // Spans fed while this flush ran sit in the queue behind it, so the
      // loop flushes once more until a flush fed nothing and held none back.
      const queuedBefore = queued
      try {
        if (!await awaitWithinBudget(processor.forceFlush().finally(() => exporter.forceFlush()))) return { drained: false, failure }
      } catch (error) {
        failure = error
      }
      if (held.length === 0 && queued === queuedBefore) return { drained: true, failure }
      // The flush emptied the queue, so held spans must fit now; if none do,
      // the processor has shut down and nothing will drain it.
      if (held.length > 0 && feedHeldSpans() === 0) return { drained: false, failure }
    }
  }

  const warnUnlessCounted = (action, failure) => {
    const cause = failure && unexplained(failure)
    if (cause) warn(`Could not ${action}: ${cause}`)
  }

  const forceFlush = async () => {
    const { drained, failure } = await drain()
    warnUnlessCounted('flush OpenTelemetry spans', failure)
    reportUndelivered({ drained })
  }

  const onExit = () => reportUndelivered({ drained: false })
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

  return { runKey, metrics, startTestSpan, finishTestSpan, forceFlush, shutdown }
}

// One session per process: a run's identity is fixed once export is
// configured, so a second configuration with a different key keeps the first
// until it is shut down.
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
