// setupFilesAfterEnv: per-test hooks a setup file registers run under each
// test, like the test file's own, with a tracer obtained as the file loads.
const { trace } = require('@opentelemetry/api')

const tracer = trace.getTracer('setup-file')

beforeEach(() => tracer.startSpan('setup-file-before').end())
afterEach(() => tracer.startSpan('setup-file-after').end())
