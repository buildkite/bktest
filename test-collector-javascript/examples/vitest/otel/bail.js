import { test, expect } from 'vitest'
test('first fails', () => expect(1).toBe(2))
test('cancelled by bail', () => {})
test('also cancelled', () => {})
