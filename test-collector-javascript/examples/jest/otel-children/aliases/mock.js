test('mapped Express mock stays untouched', () => {
  const mock = require('express')
  expect(mock).toBe(require('../support/mapped-web'))
  expect(mock.Router.use.__wrapped).toBeUndefined()
})
