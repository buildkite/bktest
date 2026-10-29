// An application module a test file requires at its top: its tracer is
// created as the module loads, before the collector registers its provider.
const http = require('http')
const { trace } = require('@opentelemetry/api')

const tracer = trace.getTracer('app')

// Each request is handled under the server's span, in a manual span.
exports.createServer = () => http.createServer((req, res) => {
  tracer.startActiveSpan('handle-request', (span) => {
    res.end('real I/O')
    span.end()
  })
})

exports.work = (name) => tracer.startSpan(name).end()
