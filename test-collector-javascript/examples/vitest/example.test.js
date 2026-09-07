import { test, describe, expect, beforeAll } from 'vitest';

// No scope
test('1 + 2 to equal 3', () => {
  expect(1 + 2).toBe(3);
});

// In a scope
describe('sum', () => {
  test('40 + 1 equal 42', () => {
    expect(40 + 1).toBe(42);
  });  
})

describe('hook fails', () => {
  beforeAll(() => { throw new Error('intentional hook failure'); });
  test('never runs', () => {});
  test.todo('todo under failed hook');
});
