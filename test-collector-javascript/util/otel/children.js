const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { warn, describeError } = require('./warn')

// Set on the context a test runs in, with the test span's trace ID, alongside
// the test span itself. Symbol.for keys are shared by every copy of
// @opentelemetry/api in every realm.
const TEST_CONTEXT = Symbol.for('buildkite.test.execution')

const FLUSH_BUDGET_MILLISECONDS = 30000

// One child stream per process: a bounded queue and an exporter labelled
// `child`, separate from test spans so that a flood, an outage or a refusal of
// child spans cannot displace, delay or fail results.
const createChildExport = ({ sdk, settings, processorSizes }) => {
  // flushBudget is the child stream's own: a child endpoint that hangs never
  // spends the time test spans need in later files.
  const metrics = { handed: 0, exported: 0, failed: 0, lastFailure: null, flushBudget: FLUSH_BUDGET_MILLISECONDS }
  let offered = 0 // ended children handed to the processor
  let late = 0 // children still open when their test file finished

  const exporter = createExporter({
    sdk, endpoint: settings.endpoint, token: settings.token,
    runKey: settings.runEnv.key, stream: 'child', metrics,
  })
  const sizes = processorSizes('CHILD', { queue: 2048, batch: 512 })
  const processor = new sdk.trace.BatchSpanProcessor(exporter, {
    maxQueueSize: sizes.queue, maxExportBatchSize: sizes.batch,
    scheduledDelayMillis: 5000, exportTimeoutMillis: EXPORT_TIMEOUT_MILLISECONDS,
  })
  const flush = () => processor.forceFlush().finally(() => exporter.forceFlush())

  // A span processor for the provider each test file's children are created
  // with. It forwards spans that belong to the trace of a test started in
  // this file, each on its own completion: spans started under a running
  // test, and spans that continue its trace through propagation, such as an
  // in-process server handling the test's request. A test's trace ID is
  // random and only reaches a span through the test, so nothing else
  // matches: setup and teardown hooks, file loading and detached root spans
  // start traces of their own.
  const createForwarder = () => {
    const traces = new Set()
    const tracked = new Set()
    let active = true
    return {
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
          // The batch processor exports with tracing suppressed, so no
          // instrumentation records the collector's own requests.
          processor.onEnd(span)
        } catch (error) {
          warn(`Could not export OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      forceFlush: () => (active ? flush() : Promise.resolve()),
      shutdown: async () => {
        active = false
        late += tracked.size
        traces.clear()
        tracked.clear()
      },
    }
  }

  // Children are best-effort, so losses are warned without the TEST RESULTS
  // MISSING wording and, as in Ruby, the first loss is reported when it is
  // noticed and the total when the process finishes, rather than after every
  // test file. A refused batch (403) counts as an export failure, and the
  // stream keeps sending.
  let warned = 0
  let finished = false
  const report = (drained, { final = false } = {}) => {
    if (finished) return
    finished = final
    const notExported = drained ? offered - metrics.handed : 0
    const lost = notExported + metrics.failed + late
    if (lost > warned && (warned === 0 || final)) {
      const cause = metrics.lastFailure ? `, last OTLP failure: ${metrics.lastFailure}` : ''
      const reasons = [
        notExported && `not-exported: ${notExported}`,
        metrics.failed && `export-failure: ${metrics.failed}${cause}`,
        late && `still open when their test file finished: ${late}`,
      ].filter(Boolean).join(', ')
      warn(`OpenTelemetry dropped ${lost} child span(s)${warned ? ' so far this run' : ''} (${reasons}); test.execution results are unaffected.`)
      warned = lost
    }
    const pending = offered - metrics.exported - metrics.failed
    if (final && !drained && pending > 0) {
      warn(`${pending} OpenTelemetry child span(s) are pending; test.execution results are unaffected.`)
    }
  }

  return {
    metrics,
    createForwarder,
    flush,
    report,
  }
}

module.exports = { createChildExport, TEST_CONTEXT }
