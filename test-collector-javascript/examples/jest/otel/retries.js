jest.retryTimes(1)

let attempts = 0
test('passes on the second attempt', () => {
  attempts += 1
  expect(attempts).toBe(2)
})

describe('retrying the entire describe', () => {
  jest.retryTimes(1, { entireDescribe: true })

  let attempts = 0
  test('passes on the second describe attempt', () => {
    attempts += 1
    expect(attempts).toBe(2)
  })
})
