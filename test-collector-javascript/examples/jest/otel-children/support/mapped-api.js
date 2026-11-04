module.exports = {
  trace: { getTracer: jest.fn(), setSpan: jest.fn() },
  context: { active: jest.fn(() => ({})), with: jest.fn((_context, fn) => fn()) },
}
