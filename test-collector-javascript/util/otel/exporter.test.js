const { createExporter } = require('./exporter')

jest.spyOn(console, 'warn').mockImplementation()

// Stands in for the SDK's OTLP exporter, settling every export with `result`.
const exportingWith = (result) => {
  const metrics = { handed: 0, exported: 0, failed: 0, lastFailure: null }
  const sdk = {
    exporter: {
      OTLPTraceExporter: class {
        export(_spans, resultCallback) { resultCallback(result) }
        shutdown() {}
      },
    },
  }
  const exporter = createExporter({ sdk, endpoint: 'http://127.0.0.1:9/v1/traces', token: 't', runKey: 'run-1', stream: 'test', metrics })
  exporter.export([{}], () => {})
  exporter.shutdown()
  return metrics
}

describe('createExporter', () => {
  it.each([
    ['a timeout', new Error('Request timed out'), 'timeout'],
    ['a timeout from exporter 0.200', new Error('Request Timeout'), 'timeout'],
    ['a rejected request', Object.assign(new Error('Forbidden'), { name: 'OTLPExporterError', code: 403 }), '403'],
    ['a network error', Object.assign(new Error('connect ECONNREFUSED 127.0.0.1:9'), { code: 'ECONNREFUSED' }), 'ECONNREFUSED'],
    ['exhausted retries with no response seen', Object.assign(new Error('Export failed with retryable status'), { name: 'OTLPExporterError' }), 'Export failed with retryable status'],
  ])('names %s as the last OTLP failure', (_case, error, cause) => {
    expect(exportingWith({ code: 1, error })).toMatchObject({ failed: 1, lastFailure: cause })
  })
})
