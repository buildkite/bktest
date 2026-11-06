const { test } = require('@playwright/test')

test('another file', async () => {
  for (let i = 0; i < 8; i++) await test.step('repeated', async () => {})
})
