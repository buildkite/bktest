/**
 * @jest-environment node
 */
test('runs under the node environment', () => {
  expect(typeof setImmediate).toBe('function')
})
