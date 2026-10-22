const name = require('path').basename(__filename)

test.each([1, 2, 3])(`passes %i in ${name}`, (n) => {
  expect(n).toBeGreaterThan(0)
})
