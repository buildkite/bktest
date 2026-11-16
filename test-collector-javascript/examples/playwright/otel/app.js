const { test: base } = require('@playwright/test')
const { test, expect, appTracingFixtures } = require('buildkite-test-collector/playwright/fixture')

const customTest = base.extend({
  request: async ({ playwright, extraHTTPHeaders }, use) => {
    const request = await playwright.request.newContext({ extraHTTPHeaders: { ...extraHTTPHeaders, 'x-custom': 'auth-present' } })
    await use(request)
    await request.dispose()
  },
}).extend(appTracingFixtures)

for (const marker of ['a', 'b', 'c', 'd']) {
  test(`page ${marker}`, async ({ page }) => {
    await page.goto(`/?marker=${marker}`)
    await expect(page.locator('#api')).toHaveText(marker)
    await expect(page.locator('#third')).toHaveText('ok')
    if (marker === 'a') {
      const headers = await page.evaluate(async () => {
        const response = await fetch('/redirect', { headers: { Authorization: 'Bearer secret' } })
        return response.json()
      })
      expect(headers).toEqual({ authorization: null, contentType: null, traceparent: null })
      const tracing = process.env.PLAYWRIGHT_OTEL_ENABLED !== 'false'
      // Firefox in the 1.41.2 floor drops route header overrides on redirects.
      const followsRedirects = test.info().project.name !== 'firefox' || require('@playwright/test/package.json').version !== '1.41.2'
      const redirected = await page.evaluate(async () => (await fetch('/redirect')).json())
      expect(redirected).toEqual({
        authorization: null,
        contentType: null,
        traceparent: tracing && followsRedirects ? expect.stringMatching(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/) : null,
      })
      const preflight = await page.evaluate(async origin => {
        try {
          await fetch(origin + '/cors', { headers: { 'x-preflight-probe': 'denied' } })
          return 'accepted'
        } catch {
          return 'blocked'
        }
      }, `http://localhost:${process.env.PLAYWRIGHT_APP_THIRD_PARTY_PORT}`)
      expect(preflight).toBe(tracing && test.info().project.name === 'chromium' ? 'accepted' : 'blocked')
    }
  })
}

customTest.describe('API', () => {
  customTest.use({ extraHTTPHeaders: { 'x-app-fixture': 'preserved' } })
  customTest('request fixture', async ({ request, page }) => {
    const response = await request.get('/api/items/r')
    expect(await response.json()).toEqual({ marker: 'r' })
    expect(response.headers()['x-app-fixture']).toBe('preserved')
    expect(response.headers()['x-custom']).toBe('auth-present')
    // A browser Request can be replayed through the API context without losing
    // its headers. Use an unsampled parent to avoid exporting unrelated spans.
    const inherited = `00-${'c'.repeat(32)}-${'d'.repeat(16)}-00`
    await page.setExtraHTTPHeaders({ Authorization: 'Bearer replay-secret', 'Content-Type': 'application/custom', traceparent: inherited })
    const original = (await page.goto(`http://localhost:${process.env.PLAYWRIGHT_APP_THIRD_PARTY_PORT}/headers`)).request()
    const replay = await request.fetch(original)
    expect(await replay.json()).toEqual({ authorization: 'Bearer replay-secret', contentType: 'application/custom', traceparent: inherited })
    const traceparent = `00-${'a'.repeat(32)}-${'b'.repeat(16)}-00`
    const explicit = await request.fetch(original, {
      method: 'POST', headers: { TraceParent: traceparent },
    })
    expect(await explicit.json()).toEqual({ authorization: null, contentType: null, traceparent })
  })
})

test('passes on retry', async ({ page }, testInfo) => {
  await page.goto(`/?marker=t${testInfo.retry}`)
  await expect(page.locator('#api')).toHaveText(`t${testInfo.retry}`)
  expect(testInfo.retry).toBe(1)
})
