/**
 * @jest-environment buildkite-test-collector/jest/environment
 */
test('runs under the collector environment', () => {
  expect(1 + 1).toBe(2)
})
