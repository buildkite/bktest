const { test } = require('@playwright/test')

// Suite hooks use separate timeout slots, outside result.duration.
test.beforeAll(async () => { await new Promise(resolve => setTimeout(resolve, 80)) })
test.afterAll(async () => { await new Promise(resolve => setTimeout(resolve, 120)) })

test('another file', async () => {
  for (let i = 0; i < 8; i++) await test.step('repeated', async () => {})
})
