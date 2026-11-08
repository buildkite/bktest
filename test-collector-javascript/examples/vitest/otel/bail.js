import { test, describe, expect } from 'vitest'
test('first fails', () => expect(1).toBe(2))
test('cancelled by bail', () => {})
describe('later suite', () => {
  test('also cancelled', () => {})
})
