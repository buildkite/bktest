const { test, expect } = require('@playwright/test')

test('API values stay private', async ({ page }) => {
  await page.setContent('<input aria-label="Password"><select multiple><option value="private-option">Private</option></select>')
  await test.step('user step "kept verbatim"', async () => {
    const input = page.getByLabel('Password')
    await input.fill('hunter2-SECRET-PASSWORD')
    await input.fill('escaped"quote\\secret')
    await input.fill('parenthesized(secret) "quoted"\\path\nnext-line')
    await input.type('typed-secret')
    await input.pressSequentially('sequential-secret')
    await input.press('Shift+S')
    await page.locator('select').selectOption(['private-option'])
    await expect(input).toBeVisible()
  })
})

test('large step errors still upload', async () => {
  await test.step('large diff', async () => {
    expect('a'.repeat(800000)).toEqual('b'.repeat(800000))
  })
})
