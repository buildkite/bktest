const diagnosticsChannel = require('diagnostics_channel')
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

const RESPONSE_CHANNEL = 'http.client.response.finish'

// Names a failed export the way the other collectors do: the HTTP status,
// "timeout", or the network error's code, falling back to the SDK's message.
// The SDK reports exhausted retries of a 429, 502, 503 or 504 without the
// status, so the exporter remembers the status of its last error response.
const failureCause = (error, lastErrorStatus) => {
  if (typeof error?.code === 'number') return String(error.code)
  if (typeof error?.code === 'string') return error.code
  if (/^Request (timed out|Timeout)$/.test(error?.message)) return 'timeout'
  if (lastErrorStatus && /retryable status/.test(error?.message)) return String(lastErrorStatus)
  if (error instanceof Error) return error.message.split('\n')[0]
  return String(error)
}

const errorClass = (error) => (error instanceof Error ? error.name : typeof error)

// Builds one OTLP/HTTP protobuf exporter for a single span stream and counts
// what passes through it, since the SDK exposes no hook for export outcomes.
// `metrics` is mutated: handed, exported, failed and lastFailure.
// `onSettled(spans, succeeded)` runs after every batch's outcome has been
// counted.
//
// The exporter's own concurrency limit (default 30) fails batches outright
// instead of queueing them; the batch processor already bounds what is in
// flight (one batch at a time on its timer, the whole queue on a flush), so
// the limit is lifted rather than turned into a reason to drop results.
//
// `runExport(send)` runs each send; the session passes one that suppresses
// tracing in every OpenTelemetry context the tests can see, so HTTP
// instrumentation never records the collector's own requests.
const createExporter = ({ sdk, endpoint, token, runKey, stream, metrics, onSettled = () => {}, runExport = (send) => send() }) => {
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

  let lastErrorStatus = null
  const recordStatus = ({ request, response }) => {
    try {
      if (response.statusCode >= 300 && request.getHeader('buildkite-tests-run-key') === runKey &&
        request.getHeader('buildkite-tests-span-stream') === stream) {
        lastErrorStatus = response.statusCode
      }
    } catch {
      // Every HTTP response in the process passes through here; none may throw.
    }
  }
  diagnosticsChannel.subscribe(RESPONSE_CHANNEL, recordStatus)

  return {
    export(spans, resultCallback) {
      metrics.handed += spans.length
      let settled = false
      const settle = (result, cause) => {
        if (settled) return
        settled = true
        const succeeded = result?.code === EXPORT_RESULT_SUCCESS
        if (succeeded) {
          metrics.exported += spans.length
          metrics.lastFailure = null
        } else {
          metrics.failed += spans.length
          metrics.lastFailure = cause ?? failureCause(result?.error, lastErrorStatus)
        }
        onSettled(spans, succeeded)
        resultCallback(result)
      }

      try {
        runExport(() => exporter.export(spans, settle))
      } catch (error) {
        // The batch processor does not catch a throwing exporter, so a throw
        // here would escape into the test run.
        const kind = errorClass(error)
        if (!warnedKinds.has(kind)) {
          warnedKinds.add(kind)
          warn(`Could not export OpenTelemetry spans: ${describeError(error)}. Further ${kind} export failures will not be reported.`)
        }
        settle({ code: EXPORT_RESULT_FAILED, error }, kind)
      }
    },
    forceFlush: () => exporter.forceFlush(),
    shutdown: () => {
      diagnosticsChannel.unsubscribe(RESPONSE_CHANNEL, recordStatus)
      return exporter.shutdown()
    },
  }
}

module.exports = { createExporter, EXPORT_TIMEOUT_MILLISECONDS }
