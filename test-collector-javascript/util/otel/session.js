const { randomBytes } = require('crypto')
const { version } = require('../../package.json')
const { loadSdk } = require('./sdk')
const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { resourceAttributes, runAttributes } = require('./attributes')
const { describeTest, truncate } = require('./describeTest')
const { warn, describeError, PREFIX } = require('./warn')

const TRACER_NAME = 'buildkite-test-collector'
const TEST_SPAN_NAME = 'test.execution'
const SPAN_STREAM = 'test'
const FLUSH_BUDGET_MILLISECONDS = 30000
const TIMED_OUT = Symbol('timed out')

// The SDK applies one value-length limit to span and event attributes alike,
// so it is set high enough for exception events (16,384) and the span's own
// string attributes are cut to 10,243 here: the server truncates test names
// to 10,240 before deriving the test ID, so a long name keeps the identity of
// its JSON upload.
const SPAN_ATTRIBUTE_VALUE_MAX_LENGTH = 10243
const SPAN_LIMITS = {
  attributeCountLimit: 128,
  attributeValueLengthLimit: 16384,
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

const missingResults = (count, detail) => {
  warn(
    `TEST RESULTS MISSING: OpenTelemetry dropped ${count} test.execution span(s) (${detail}).\n` +
    `${PREFIX} OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.`
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
  const reported = { failed: 0, notExported: 0 }

  const sizes = processorSizes()
  const exporter = createExporter({
    sdk,
    endpoint: settings.endpoint,
    token: settings.token,
    runKey,
    stream: SPAN_STREAM,
    metrics,
  })
  const processor = new trace.BatchSpanProcessor(exporter, {
    maxQueueSize: sizes.queue,
    maxExportBatchSize: sizes.batch,
    scheduledDelayMillis: SCHEDULE_DELAY_MILLISECONDS,
    exportTimeoutMillis: EXPORT_TIMEOUT_MILLISECONDS,
  })
  const resource = resources.defaultResource()
    .merge(resources.detectResources({ detectors: [resources.envDetector] }))
    .merge(resources.resourceFromAttributes(resourceAttributes(runEnv)))
  const provider = new trace.BasicTracerProvider({
    sampler: new trace.AlwaysOnSampler(),
    idGenerator,
    resource,
    spanLimits: SPAN_LIMITS,
    spanProcessors: [processor],
  })
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

  const reportDrops = ({ drained }) => {
    const failed = metrics.failed - reported.failed
    if (failed > 0) {
      reported.failed = metrics.failed
      const cause = metrics.lastFailure ? `, last OTLP failure: ${metrics.lastFailure}` : ''
      missingResults(failed, `export-failure${cause}`)
    }
    if (drained) {
      const notExported = metrics.ended - metrics.handed - reported.notExported
      if (notExported > 0) {
        reported.notExported += notExported
        missingResults(notExported, 'not-exported')
      }
    }
  }

  // The processor rejects a flush when any batch in it failed, which the
  // exporter wrapper has already counted; only an unexpected failure is worth
  // its own warning.
  const unexplained = (error) => metrics.failed === reported.failed && describeError(error)

  // The processor's flush only covers the queue; a batch its timer already
  // handed to the exporter (retries included) is joined through the exporter.
  const forceFlush = async () => {
    let drained = false
    try {
      drained = await awaitWithinBudget(processor.forceFlush().finally(() => exporter.forceFlush()))
    } catch (error) {
      const cause = unexplained(error)
      if (cause) warn(`Could not flush OpenTelemetry spans: ${cause}`)
    }
    reportDrops({ drained })
  }

  const onExit = () => {
    reportDrops({ drained: false })
    const pending = metrics.ended - metrics.exported - metrics.failed - reported.notExported
    if (pending > 0) {
      warn(
        `TEST RESULTS MISSING: ${pending} test.execution span(s) were still queued or being exported when the process exited; ` +
        'those test executions were not uploaded to Buildkite Test Engine.'
      )
    }
  }
  process.on('exit', onExit)

  let shutDown = false
  const shutdown = async () => {
    if (shutDown) return
    shutDown = true
    let drained = false
    try {
      drained = await awaitWithinBudget(provider.shutdown())
    } catch (error) {
      const cause = unexplained(error)
      if (cause) warn(`Could not shut down OpenTelemetry span export: ${cause}`)
    }
    reportDrops({ drained })
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
