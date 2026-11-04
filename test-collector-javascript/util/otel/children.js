const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { warn, describeError } = require('./warn')

const FLUSH_BUDGET_MILLISECONDS = 30000

// Child floods and export failures must not displace or delay test results.
const createChildExport = ({ sdk, settings, processorSizes }) => {
  const metrics = { handed: 0, exported: 0, failed: 0, lastFailure: null, flushBudget: FLUSH_BUDGET_MILLISECONDS }
  let offered = 0 // ended children handed to the processor
  let open = 0 // accepted children that have not ended yet
  let startedAfterTeardown = 0
  // Retain trace IDs, not closed sandboxes, to recognise late descendants.
  const closedTraces = new Set()

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

  // Trace membership includes propagated server spans, but excludes unrelated work.
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
          open += 1
        } catch (error) {
          warn(`Could not track OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      onEnd(span) {
        try {
          if (!tracked.delete(span.spanContext().spanId)) return
          open -= 1
          offered += 1
          processor.onEnd(span)
          // There may be no later file teardown to flush this completion.
          if (!active) flush().catch((error) => warn(`Could not flush OpenTelemetry child span: ${describeError(error)}`))
        } catch (error) {
          warn(`Could not export OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      shutdown: async () => {
        active = false
        for (const traceId of traces) closedTraces.add(traceId)
        traces.clear()
      },
    }
  }

  let warned = 0
  let finished = false
  let notExported = 0
  const report = (drained, { final = false } = {}) => {
    if (finished) return
    finished = final
    // Only a completed flush distinguishes queue drops from spans still waiting.
    if (drained) notExported = offered - metrics.handed
    const late = final ? open : 0
    const lost = notExported + metrics.failed + startedAfterTeardown + late
    if (lost > warned && (warned === 0 || final)) {
      const cause = metrics.lastFailure ? `, last OTLP failure: ${metrics.lastFailure}` : ''
      const reasons = [
        notExported && `not-exported: ${notExported}`,
        metrics.failed && `export-failure: ${metrics.failed}${cause}`,
        startedAfterTeardown && `started after file teardown: ${startedAfterTeardown}`,
        late && `still open when the worker exited: ${late}`,
      ].filter(Boolean).join(', ')
      warn(`OpenTelemetry dropped ${lost} child span(s)${warned ? ' so far this run' : ''} (${reasons}); test.execution results are unaffected.`)
      warned = lost
    }
    const pending = offered - metrics.exported - metrics.failed - notExported
    if (final && !drained && pending > 0) {
      warn(`${pending} OpenTelemetry child span(s) are pending; test.execution results are unaffected.`)
    }
  }

  return {
    metrics,
    createForwarder,
    isClosedTrace: (traceId) => closedTraces.has(traceId),
    rejectLate(span) {
      if (!closedTraces.has(span.spanContext().traceId)) return false
      startedAfterTeardown += 1
      return true
    },
    flush,
    report,
  }
}

module.exports = { createChildExport }
