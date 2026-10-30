import { test, expect } from 'vitest';

let attempts = 0;

test('passes on retry', () => {
  attempts += 1;
  expect(attempts).toBe(2);
});

test('fails every attempt', () => {
  expect(1).toBe(2);
});
