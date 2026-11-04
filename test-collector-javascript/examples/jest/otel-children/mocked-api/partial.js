jest.mock('@opentelemetry/api', () => ({
  ...jest.requireActual('@opentelemetry/api'),
  trace: { getTracer: () => ({ startSpan: jest.fn() }) },
}))
require('../support/mockedApiRoute')()
