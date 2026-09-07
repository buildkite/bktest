import { it, describe, expect } from 'vitest';

describe('passed', () => {
  it('is true', () => {
    expect(true).toBeTruthy()
  });

  describe('nested', () => {
    it.skip('is skipped', () => {});
    it.todo('is pending');
    it('is skipped at runtime', ({ skip }) => {
      skip();
    });
  });
})
