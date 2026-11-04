jest.mock('@opentelemetry/api')
const api = require('@opentelemetry/api')

test('leaves API mocks alone', () => {
  require('express')
  expect(api.trace.getTracerProvider).not.toHaveBeenCalled()
  expect(api.context.active).not.toHaveBeenCalled()
})
