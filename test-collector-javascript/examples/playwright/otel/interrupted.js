const { test } = require('@playwright/test')

test('cancelled by SIGINT', async () => {
  // The worker's parent is this fixture's Playwright runner, not the outer test harness.
  process.kill(process.ppid, 'SIGINT')
  await new Promise(() => {})
})
