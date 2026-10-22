const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { warn, describeError } = require('./warn')

// This key crosses the environment/sandbox boundary without sharing either
// realm's global OTel registry.
const TEST_CONTEXT = Symbol.for('buildkite.test.execution')

const createChildExport = ({ sdk, settings, providerConfig, processorSizes }) => {
  const metrics = { handed: 0, exported: 0, failed: 0, lastFailure: null }
  let offered = 0
  let reported = 0
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
  const forwarders = new Set()
  const createForwarder = (api) => {
    const tracked = new Set()
    let active = true
    const forwarder = {
      onStart(span, parentContext) {
        try {
          if (!active || parentContext.getValue(TEST_CONTEXT) !== span.spanContext().traceId) return
          tracked.add(span.spanContext().spanId)
        } catch (error) {
          warn(`Could not track OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      onEnd(span) {
        try {
          const id = span.spanContext().spanId
          if (!active || !tracked.delete(id)) return
          offered += 1
          // A full batch can export synchronously here. Schedule transport
          // outside the sandbox's test context: the exporter's own OTel
          // suppression lives in the environment's separate API realm.
          api.context.with(api.ROOT_CONTEXT, () => processor.onEnd(span))
        } catch (error) {
          warn(`Could not export OpenTelemetry child span: ${describeError(error)}`)
        }
      },
      forceFlush: () => active ? api.context.with(api.ROOT_CONTEXT, flush) : Promise.resolve(),
      shutdown: async () => { active = false; tracked.clear(); forwarders.delete(forwarder) },
    }
    forwarders.add(forwarder)
    return forwarder
  }

  const report = (drained) => {
    const lost = metrics.failed + (drained ? offered - metrics.handed : 0)
    if (lost > reported) {
      warn(`OpenTelemetry dropped ${lost - reported} child span(s) (not-exported or export-failure${metrics.lastFailure ? `, last OTLP failure: ${metrics.lastFailure}` : ''}); test.execution results are unaffected.`)
      reported = lost
    }
    const pending = offered - metrics.exported - metrics.failed
    if (!drained && pending > 0) {
      warn(`${pending} OpenTelemetry child span(s) are pending; test.execution results are unaffected.`)
    }
  }

  return {
    createForwarder,
    createProvider: (trace, forwarder) => new trace.BasicTracerProvider({
      ...providerConfig, sampler: undefined, spanProcessors: [forwarder],
    }),
    flush,
    deactivate: () => { for (const f of forwarders) f.shutdown(); forwarders.clear() },
    shutdown: () => processor.shutdown(),
    report,
  }
}

module.exports = { createChildExport, TEST_CONTEXT }
