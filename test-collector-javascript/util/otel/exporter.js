const diagnosticsChannel = require('diagnostics_channel')
const { warn, describeError } = require('./warn')

const EXPORT_TIMEOUT_MILLISECONDS = 30000
const EXPORT_RESULT_SUCCESS = 0
const EXPORT_RESULT_FAILED = 1

// The SDK merges env headers into explicit headers. Don't leak another
// destination's credentials into Buildkite requests.
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

// Exhausted SDK retries omit the status; recover it from the last response.
const failureCause = (error, lastErrorStatus) => {
  if (typeof error?.code === 'number') return String(error.code)
  if (typeof error?.code === 'string') return error.code
  if (/^Request (timed out|Timeout)$/.test(error?.message)) return 'timeout'
  if (lastErrorStatus && /retryable status/.test(error?.message)) return String(lastErrorStatus)
  if (error instanceof Error) return error.message.split('\n')[0]
  return String(error)
}

const createExporter = ({ sdk, endpoint, token, runKey, stream, metrics, onSettled = () => {} }) => {
  const exporter = withoutExporterEnv(() => new sdk.exporter.OTLPTraceExporter({
    url: endpoint,
    headers: {
      Authorization: `Token token="${token}"`,
      'Buildkite-Tests-Run-Key': runKey,
      'Buildkite-Tests-Span-Stream': stream,
    },
    compression: 'gzip',
    timeoutMillis: EXPORT_TIMEOUT_MILLISECONDS,
    // The processor already bounds batches; another limit would drop results.
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
        exporter.export(spans, settle)
      } catch (error) {
        // The batch processor does not catch exporter throws.
        const kind = error instanceof Error ? error.name : typeof error
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
