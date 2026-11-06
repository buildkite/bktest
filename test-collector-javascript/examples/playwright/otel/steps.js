const { test: base, expect } = require('@playwright/test')
const { trace } = require('@opentelemetry/api')

const test = base.extend({
  value: async ({}, use) => {
    await test.step('fixture setup', async () => {})
    await use(42)
    await test.step('fixture teardown', async () => {})
  },
})

test.beforeAll(async () => { await test.step('before all', async () => {}) })
test.beforeEach(async () => { await test.step('before each', async () => {}) })
test.afterEach(async () => { await test.step('after each', async () => {}) })
test.afterAll(async () => { await test.step('after all', async () => {}) })

for (const number of [1, 2]) {
  test(`nested parallel ${number}`, async ({ page, value }) => {
    expect(value).toBe(42)
    expect(trace.getActiveSpan()).toBeUndefined()
    // Reporter-side children must not collect arbitrary manual worker spans.
    trace.getTracer('worker').startSpan('not a reporter step').end()
    await page.setContent('<button>Run</button>')
    await test.step('outer', async () => {
      await Promise.all([40, 10].map(delay => test.step('repeated', async () => {
        await new Promise(resolve => setTimeout(resolve, delay))
        await test.step('leaf', async () => { expect(delay).toBeGreaterThan(0) })
      })))
      await test.step('repeated', async () => { await page.getByRole('button', { name: 'Run' }).click() })
      await test.step('repeated', async () => {})
    })
  })
}

test.describe('retry steps', () => {
  test.describe.configure({ retries: 1 })
  test('retry child', async ({}, info) => {
    await test.step('retry step', async () => { expect(info.retry).toBe(1) })
  })
})

test('skip inside step', async () => {
  await test.step('skip step', async () => { test.skip() })
})

test.skip('static skip', async () => {})
