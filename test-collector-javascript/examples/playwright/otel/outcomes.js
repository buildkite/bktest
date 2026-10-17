const { test, expect } = require('@playwright/test')

test('passes with tags', { tag: ['@priority:test', '@ci.worker.id:test-worker', '@ignored'] }, async () => {
  await new Promise(resolve => setTimeout(resolve, 25))
  expect(1 + 2).toBe(3)
})

test('fails', () => { throw new Error('intentional failure') })
test.skip('skipped', () => {})

test.describe('retry', () => {
  test.describe.configure({ retries: 1 })
  test('passes on retry', ({}, info) => { expect(info.retry).toBe(1) })
})

test('expected failure', () => {
  test.fail()
  throw new Error('expected failure')
})

test('unexpected pass', () => { test.fail() })

test('times out', async () => {
  test.setTimeout(50)
  await new Promise(() => {})
})

test.describe('beforeAll fails', () => {
  test.beforeAll(() => { throw new Error('beforeAll failure') })
  test('first', () => {})
  test('second', () => {})
})

test.describe('afterEach fails', () => {
  test.afterEach(() => { throw new Error('afterEach failure') })
  test('body passes', () => {})
})

for (const value of [3, 7]) {
  test(`parameter ${value}`, () => { expect(value).toBeGreaterThan(0) })
}
