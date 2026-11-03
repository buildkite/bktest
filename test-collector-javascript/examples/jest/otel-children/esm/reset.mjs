// Native ESM (--experimental-vm-modules), where Jest loads the API's
// CommonJS build for an import: a tracer obtained at load, before a reset in
// beforeAll, records children.
import { trace } from '@opentelemetry/api'

const tracer = trace.getTracer('esm')

beforeAll(() => {
  import.meta.jest.resetModules()
})

test('esm after reset', () => {
  tracer.startSpan('esm-child').end()
})
