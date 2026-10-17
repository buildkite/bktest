const { describeTest } = require('./describeTest')

const record = (overrides = {}) => ({
  id: 'id-1',
  scope: 'sum',
  name: 'adds',
  location: 'src/sum.test.js:12',
  file_name: 'src/sum.test.js',
  result: 'passed',
  failure_reason: undefined,
  failure_expanded: [{ expanded: [] }],
  ...overrides,
})

describe('describeTest', () => {
  it('derives the identity attributes from the JSON record', () => {
    expect(describeTest(record(), { line: 12 }).attributes).toEqual({
      'buildkite.test.scope': 'sum',
      'buildkite.test.name': 'adds',
      'test.case.name': 'sum adds',
      'test.suite.name': 'sum',
      'code.file.path': 'src/sum.test.js',
      'code.line.number': 12,
      'buildkite.test.execution.external_id': 'id-1',
    })
  })

  it('omits the line number unless it is an integer and keeps an empty scope', () => {
    const { attributes } = describeTest(record({ scope: '' }), { line: undefined })

    expect(attributes['code.line.number']).toBeUndefined()
    expect(attributes['buildkite.test.scope']).toBe('')
    expect(attributes['test.case.name']).toBe('adds')
    expect(describeTest(record(), { line: '12' }).attributes['code.line.number']).toBeUndefined()
  })

  it('includes an optional integer column without inventing one when absent', () => {
    expect(describeTest(record(), { line: 12, column: 7 }).attributes['code.column.number']).toBe(7)
    expect(describeTest(record(), { line: 12 }).attributes['code.column.number']).toBeUndefined()
    expect(describeTest(record(), { column: '7' }).attributes['code.column.number']).toBeUndefined()
  })

  it('maps every Test Engine result to a span status', () => {
    expect(describeTest(record({ result: 'passed' })).status).toBe('pass')
    expect(describeTest(record({ result: 'failed' })).status).toBe('fail')
    expect(describeTest(record({ result: 'pending' })).status).toBe('skipped')
    expect(describeTest(record({ result: 'skipped' })).status).toBe('skipped')
    expect(describeTest(record({ result: 'unknown' })).status).toBeUndefined()
  })

  it('carries the failure reason and one exception event per expanded failure', () => {
    const described = describeTest(record({
      result: 'failed',
      failure_reason: 'expect(received).toBe(expected)',
      failure_expanded: [{ expanded: ['Expected: 2', 'Received: 3'] }, { expanded: [] }, { expanded: ['second'] }],
    }))

    expect(described.failureReason).toBe('expect(received).toBe(expected)')
    expect(described.exceptionEvents).toEqual([
      { name: 'exception', attributes: { 'exception.message': 'Expected: 2\nReceived: 3' } },
      { name: 'exception', attributes: { 'exception.message': 'second' } },
    ])
  })

  it('truncates the reason to 1024 and the exception message to 10243 characters', () => {
    const described = describeTest(record({
      result: 'failed',
      failure_reason: 'r'.repeat(2000),
      failure_expanded: [{ expanded: ['m'.repeat(20000)] }],
    }))

    expect(described.failureReason).toHaveLength(1024)
    expect(described.failureReason.endsWith('...')).toBe(true)
    expect(described.exceptionEvents[0].attributes['exception.message']).toHaveLength(10243)
    expect(described.exceptionEvents[0].attributes['exception.message'].endsWith('...')).toBe(true)
  })

  it('truncates by code point, so an emoji counts once and is never cut in half', () => {
    // 1,021 code points are kept before '...'; the emoji is the 1,021st.
    const reason = `${'r'.repeat(1020)}😀${'r'.repeat(1000)}`
    const described = describeTest(record({ result: 'failed', failure_reason: reason }))

    expect(described.failureReason).toBe(`${'r'.repeat(1020)}😀...`)
    expect(Array.from(described.failureReason)).toHaveLength(1024)
    expect(described.failureReason).not.toMatch(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])/)

    // A value within the limit in code points stays whole even when its UTF-16 length exceeds it.
    const emoji = '😀'.repeat(1024)
    expect(describeTest(record({ result: 'failed', failure_reason: emoji })).failureReason).toBe(emoji)
  })

  it('records no failure details for a passing test even if the record carries some', () => {
    const described = describeTest(record({ result: 'passed', failure_reason: 'stale', failure_expanded: [{ expanded: ['x'] }] }))

    expect(described.failureReason).toBeUndefined()
    expect(described.exceptionEvents).toEqual([])
  })
})
