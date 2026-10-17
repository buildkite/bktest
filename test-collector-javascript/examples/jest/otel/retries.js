jest.retryTimes(1)

let attempts = 0
test('passes on the second attempt', () => {
  attempts += 1
  expect(attempts).toBe(2)
})
