// Covers the outcomes the OTel path has to map, beyond the plain pass/fail
// in example.test.js.
describe('outcomes', () => {
  test.skip('skipped by test.skip', () => {})
  test.todo('still to write')

  test('passes with fake timers', () => {
    jest.useFakeTimers({ doNotFake: ['performance'] })
    try {
      jest.setSystemTime(new Date('2000-01-01T00:00:00Z'))
      expect(Date.now()).toBe(946684800000)
    } finally {
      jest.useRealTimers()
    }
  })

  test('throws a non-error value', () => {
    throw { reason: 'plain object' }
  })

  test.each([[1, 1], [2, 3]])('each %i + %i', (a, b) => {
    expect(a + b).toBe(2)
  })
})

// jest-circus names this test ROOT_DESCRIBE_BLOCK with no ancestors.
describe('ordinary', () => {
  test('ROOT_DESCRIBE_BLOCK', () => {})
})

describe('with a failing beforeAll', () => {
  beforeAll(() => { throw new Error('beforeAll failed') })
  test('never runs', () => {})
  test('also never runs', () => {})
})
