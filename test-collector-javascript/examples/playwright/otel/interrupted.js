const { test } = require('@playwright/test')

test('interrupted', async () => {
  // The worker's parent is this fixture's Playwright runner, not the outer test harness.
  process.kill(process.ppid, 'SIGINT')
  await new Promise(() => {})
})
