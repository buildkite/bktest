const { warn, describeError } = require('./warn')

const EXPORT_TIMEOUT_MILLISECONDS = 30000
const EXPORT_RESULT_SUCCESS = 0
const EXPORT_RESULT_FAILED = 1

// The SDK's OTLP exporter reads OTEL_EXPORTER_OTLP_* when it is constructed
// and merges env headers into explicit ones, so bktec's relay token or a
// vendor's auth header would leak into the Buildkite request. Only
// BUILDKITE_* variables may configure this exporter.
const withoutExporterEnv = (build) => {
  const removed = {}
  for (const key of Object.keys(process.env)) {
    if (key.startsWith('OTEL_EXPORTER_OTLP_')) {
      removed[key] = process.env[key]
      delete process.env[key]
    }
  }
  try {
    return build()
  } finally {
    Object.assign(process.env, removed)
  }
}

const failureCause = (error) => {
  if (error && error.code !== undefined && error.code !== null) return String(error.code)
  if (error instanceof Error) return error.name
  return describeError(error)
}

// Builds one OTLP/HTTP protobuf exporter for a single span stream and counts
// what passes through it, since the SDK exposes no hook for export outcomes.
// `metrics` is mutated: handed, exported, failed and lastFailure.
//
// The exporter's own concurrency limit (default 30) fails batches outright
// instead of queueing them; the batch processor already bounds what is in
// flight (one batch at a time on its timer, the whole queue on a flush), so
// the limit is lifted rather than turned into a reason to drop results.
const createExporter = ({ sdk, endpoint, token, runKey, stream, metrics }) => {
  const exporter = withoutExporterEnv(() => new sdk.exporter.OTLPTraceExporter({
    url: endpoint,
    headers: {
      Authorization: `Token token="${token}"`,
      'Buildkite-Tests-Run-Key': runKey,
      'Buildkite-Tests-Span-Stream': stream,
    },
    compression: 'gzip',
    timeoutMillis: EXPORT_TIMEOUT_MILLISECONDS,
    concurrencyLimit: Infinity,
  }))
  const warnedKinds = new Set()

  return {
    export(spans, resultCallback) {
      metrics.handed += spans.length
      let settled = false
      const settle = (result) => {
        if (settled) return
        settled = true
        if (result && result.code === EXPORT_RESULT_SUCCESS) {
          metrics.exported += spans.length
          metrics.lastFailure = null
        } else {
          metrics.failed += spans.length
          metrics.lastFailure = failureCause(result && result.error)
        }
        resultCallback(result)
      }

      try {
        exporter.export(spans, settle)
      } catch (error) {
        // The batch processor does not catch a throwing exporter, so a throw
        // here would escape into the test run.
        const kind = failureCause(error)
        if (!warnedKinds.has(kind)) {
          warnedKinds.add(kind)
          warn(`Could not export OpenTelemetry spans: ${describeError(error)}. Further ${kind} export failures will not be reported.`)
        }
        settle({ code: EXPORT_RESULT_FAILED, error })
      }
    },
    forceFlush: () => exporter.forceFlush(),
    shutdown: () => exporter.shutdown(),
  }
}

module.exports = { createExporter, EXPORT_TIMEOUT_MILLISECONDS }
