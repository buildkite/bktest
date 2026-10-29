// Preloaded with --require: an instrumentation package's require hook sees
// Node's http module before any instrumentation registers for it, as happens
// when a suite or tool loads one disabled. require-in-the-middle then never
// hands http to instrumentation registered later.
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http')

new HttpInstrumentation({ enabled: false }) // eslint-disable-line no-new
require('http')
