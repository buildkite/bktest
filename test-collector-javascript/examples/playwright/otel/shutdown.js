const { test } = require('@playwright/test')

for (let i = 0; i < 6; i++) test(`completed ${i}`, async () => {})

test('open during shutdown', async () => {
  await test.step('unfinished at shutdown', async () => {
    if (process.env.PLAYWRIGHT_OTEL_SIGNAL) {
      require('fs').writeFileSync(process.env.PLAYWRIGHT_JSON_OUTPUT_NAME + '.signal', String(Date.now()))
      process.kill(process.ppid, process.env.PLAYWRIGHT_OTEL_SIGNAL)
    }
    await new Promise(() => {})
  })
})
