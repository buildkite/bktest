const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { warn, warnOncePerRun, describeError } = require('./warn')

// Set on the context a test runs in, with the test span's trace ID, alongside
// the test span itself. Symbol.for keys are shared by every copy of
// @opentelemetry/api in every realm.
const TEST_CONTEXT = Symbol.for('buildkite.test.execution')

const FLUSH_BUDGET_MILLISECONDS = 30000
const REFUSED_STATUS = '403'
const EXPORT_RESULT_FAILED = 1

// One child stream per process: a bounded queue and an exporter labelled
// `child`, separate from test spans so that a flood, an outage or a refusal of
// child spans cannot displace, delay or fail results.
const createChildExport = ({ sdk, settings, processorSizes, inCollectorContext }) => {
  // flushBudget is the child stream's own: a child endpoint that hangs never
  // spends the time test spans need in later files.
  const metrics = { handed: 0, exported: 0, failed: 0, lastFailure: null, flushBudget: FLUSH_BUDGET_MILLISECONDS }
  let offered = 0 // ended children handed to the processor
  let refusedSent = 0 // handed spans the receiver answered 403 to (also counted as failed)
  let skipped = 0 // spans dropped without a request after the refusal
  let reported = 0
  let refused = false
  const forwarders = new Set()

  // Buildkite answers 403 to child requests once the organization is over its
  // child span limit, and to every later one, so the stream stops and the run
  // warns once, whichever worker hits it first.
  const refuse = () => {
    if (refused) return
    refused = true
    for (const forwarder of [...forwarders]) forwarder.shutdown()
    warnOncePerRun(settings.exportRecords, 'child-spans-refused',
      'Buildkite Test Engine refused OpenTelemetry child spans (HTTP 403), for example because the organization has reached its child span limit; ' +
      'child span export has stopped. test.execution results are unaffected.')
  }

  const exporter = createExporter({
    sdk, endpoint: settings.endpoint, token: settings.token,
    runKey: settings.runEnv.key, stream: 'child', metrics, runExport: inCollectorContext,
    onSettled: (spans, succeeded) => {
      if (succeeded || metrics.lastFailure !== REFUSED_STATUS) return
      refusedSent += spans.length
      refuse()
    },
  })
  const gatedExporter = {
    export(spans, resultCallback) {
      if (!refused) return exporter.export(spans, resultCallback)
      skipped += spans.length
      resultCallback({ code: EXPORT_RESULT_FAILED, error: new Error('child spans refused') })
    },
    forceFlush: () => exporter.forceFlush(),
    shutdown: () => exporter.shutdown(),
  }
  const sizes = processorSizes('CHILD', { queue: 2048, batch: 512 })
  const processor = new sdk.trace.BatchSpanProcessor(gatedExporter, {
    maxQueueSize: sizes.queue, maxExportBatchSize: sizes.batch,
    scheduledDelayMillis: 5000, exportTimeoutMillis: EXPORT_TIMEOUT_MILLISECONDS,
  })
  const flush = () => inCollectorContext(() => processor.forceFlush()).finally(() => exporter.forceFlush())

  // A span processor for whichever provider creates children (the
  // collector's or the suite's). It forwards spans that belong to the trace of
  // a test started in this file, each on its own completion: spans started
  // under a running test, and spans that continue its trace through
  // propagation, such as an in-process server handling the test's request.
  // A test's trace ID is random and only reaches a span through the test, so
  // nothing else matches: setup and teardown hooks, file loading and detached
  // root spans start traces of their own.
  const createForwarder = () => {
    const traces = new Set()
    const tracked = new Set()
    let active = !refused
    const forwarder = {
      addTrace(traceId) {
        if (active) traces.add(traceId)
      },
      onStart(span) {
        try {
          if (!active || !traces.has(span.spanContext().traceId)) return
          tracked.add(span.spanContext().spanId)
        } catch (error) {
          warn(`Could not track OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      onEnd(span) {
        try {
          if (!tracked.delete(span.spanContext().spanId) || !active) return
          offered += 1
          // A full batch exports synchronously from here, and a timer it
          // starts inherits this context: neither may run as the test.
          inCollectorContext(() => processor.onEnd(span))
        } catch (error) {
          warn(`Could not export OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      forceFlush: () => (active ? flush() : Promise.resolve()),
      shutdown: async () => {
        active = false
        traces.clear()
        tracked.clear()
        forwarders.delete(forwarder)
      },
    }
    if (active) forwarders.add(forwarder)
    return forwarder
  }

  // Children are best-effort, so losses are warned without the TEST RESULTS
  // MISSING wording, and, as in Ruby, the first loss is reported when it is
  // noticed and later ones as a total when the process finishes, rather than
  // after every test file. Refused spans were covered by the refusal warning.
  let warnedLoss = false
  let finished = false
  const report = (drained, { final = false } = {}) => {
    if (finished) return
    finished = final
    const neverReached = drained ? offered - metrics.handed - skipped : 0
    const lost = metrics.failed - refusedSent + neverReached
    if (lost > reported && (!warnedLoss || final)) {
      const cause = metrics.lastFailure && metrics.lastFailure !== REFUSED_STATUS ? `, last OTLP failure: ${metrics.lastFailure}` : ''
      warn(`OpenTelemetry dropped ${lost - reported} child span(s) (not-exported or export-failure${cause}); test.execution results are unaffected.`)
      reported = lost
      warnedLoss = true
    }
    const pending = offered - metrics.exported - metrics.failed - skipped
    if (final && !drained && !refused && pending > 0) {
      warn(`${pending} OpenTelemetry child span(s) are pending; test.execution results are unaffected.`)
    }
  }

  return {
    metrics,
    createForwarder,
    flush,
    deactivate: () => { for (const forwarder of [...forwarders]) forwarder.shutdown() },
    shutdown: () => processor.shutdown(),
    report,
  }
}

module.exports = { createChildExport, TEST_CONTEXT }
