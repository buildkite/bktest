const diagnosticsChannel = require('diagnostics_channel')
const { warnOncePerRun, describeError } = require('./warn')

// Keep the stock timeout; shutdown also aborts sockets, including connects.
const EXPORT_TIMEOUT_MILLISECONDS = 10000
const RESPONSE_CHANNEL = 'http.client.response.finish'

// Do not merge another OTLP destination's credentials into our headers.
const withoutExporterEnv = (build) => {
  const removed = {}
  for (const key of Object.keys(process.env)) {
    if (key.startsWith('OTEL_EXPORTER_OTLP_')) {
      removed[key] = process.env[key]
      delete process.env[key]
    }
  }
  try { return build() } finally { Object.assign(process.env, removed) }
}

const failureCause = (error, lastErrorStatus) => {
  if (typeof error?.code === 'number' || typeof error?.code === 'string') return String(error.code)
  if (/^Request (timed out|Timeout)$/.test(error?.message)) return 'timeout'
  if (lastErrorStatus && /retryable status/.test(error?.message)) return String(lastErrorStatus)
  return error instanceof Error ? error.message.split('\n')[0] : String(error)
}

const createExporter = ({ sdk, endpoint, token, runKey, stream, metrics, directory }) => {
  const controller = new AbortController()
  const { Agent } = require(new URL(endpoint).protocol === 'https:' ? 'https' : 'http')
  const agent = new Agent({ keepAlive: true, timeout: EXPORT_TIMEOUT_MILLISECONDS, signal: controller.signal })
  let lastErrorStatus
  const addRequest = agent.addRequest.bind(agent)
  agent.addRequest = (request, ...args) => {
    request.once('error', (error) => { metrics.lastFailure = failureCause(error, lastErrorStatus) })
    return addRequest(request, ...args)
  }
  const exporter = withoutExporterEnv(() => new sdk.exporter.OTLPTraceExporter({
    url: endpoint,
    headers: {
      Authorization: `Token token="${token}"`,
      'Buildkite-Tests-Run-Key': runKey,
      'Buildkite-Tests-Span-Stream': stream,
    },
    compression: 'gzip', timeoutMillis: EXPORT_TIMEOUT_MILLISECONDS, concurrencyLimit: Infinity,
    // Keep the agent even if a refused connection fails before Node 18's
    // request.start diagnostic. Both supported exporter lines accept a factory.
    httpAgentOptions: () => agent,
  }))
  const matches = (request) => request.getHeader('buildkite-tests-run-key') === runKey &&
    request.getHeader('buildkite-tests-span-stream') === stream
  const recordStatus = ({ request, response }) => {
    try {
      if (response.statusCode >= 300 && matches(request)) {
        lastErrorStatus = response.statusCode
        metrics.lastFailure = String(response.statusCode)
        const value = response.headers['retry-after']
        const seconds = Number(value)
        if (value !== undefined) metrics.retryAt = Number.isFinite(seconds)
          ? Date.now() + Math.max(0, seconds * 1000) : Date.parse(value)
      }
    } catch { /* HTTP diagnostics must never break the test run. */ }
  }
  diagnosticsChannel.subscribe(RESPONSE_CHANNEL, recordStatus)

  return {
    export(spans, resultCallback) {
      metrics.handed += spans.length
      let settled = false
      const settle = (result) => {
        if (settled) return
        settled = true
        if (result?.code === 0) {
          metrics.exported += spans.length
          metrics.lastFailure = null
        } else {
          metrics.failed += spans.length
          metrics.lastFailure = failureCause(result?.error, lastErrorStatus)
        }
        resultCallback(result)
      }
      try {
        // BSP normally suppresses export tracing. There is no BSP in the parent.
        sdk.api.context.with(sdk.core.suppressTracing(sdk.api.ROOT_CONTEXT), () => exporter.export(spans, settle))
      } catch (error) {
        warnOncePerRun(directory, `export-${error.name}`, `Could not export OpenTelemetry spans: ${describeError(error)}`)
        settle({ code: 1, error })
      }
    },
    shutdown() {
      // A queued stock retry must fail without constructing a Socket with an
      // already-aborted signal (which can emit an unhandled Socket error).
      agent.createConnection = (_options, callback) => {
        process.nextTick(callback, Object.assign(new Error('Export drain ended'), { code: 'ABORT_ERR' }))
      }
      controller.abort()
      diagnosticsChannel.unsubscribe(RESPONSE_CHANNEL, recordStatus)
      return exporter.shutdown()
    },
  }
}

module.exports = { createExporter, EXPORT_TIMEOUT_MILLISECONDS }
