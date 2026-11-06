const { test } = require('@playwright/test')

test('cancelled by SIGINT', async () => {
  await test.step('completed before cancellation', async () => {})
  await test.step('interrupted outer', async () => {
    await test.step('interrupted inner', async () => {
      // The worker's parent is this fixture's Playwright runner, not the outer test harness.
      process.kill(process.ppid, 'SIGINT')
      await new Promise(() => {})
    })
  })
})
