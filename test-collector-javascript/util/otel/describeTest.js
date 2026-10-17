const FAILURE_REASON_MAX_LENGTH = 1024
const EXCEPTION_MESSAGE_MAX_LENGTH = 10243

// Unlisted results keep the 'unset' placeholder, which the server skips.
const STATUSES = {
  passed: 'pass',
  failed: 'fail',
  pending: 'skipped',
  skipped: 'skipped',
}

// Count code points, not UTF-16 units, to preserve JSON test identity with emoji.
const truncate = (text, max, omission = '...') => {
  if (text.length <= max) return text
  const codePoints = Array.from(text)
  if (codePoints.length <= max) return text
  return `${codePoints.slice(0, max - omission.length).join('')}${omission}`
}

const testAttributes = (record, line, column) => ({
  'buildkite.test.scope': record.scope,
  'buildkite.test.name': record.name,
  'test.case.name': [record.scope, record.name].filter(Boolean).join(' '),
  'test.suite.name': record.scope,
  'code.file.path': record.file_name,
  'code.line.number': Number.isInteger(line) ? line : undefined,
  'code.column.number': Number.isInteger(column) ? column : undefined,
  'buildkite.test.execution.external_id': record.id,
})

const exceptionEvents = (record) => {
  const events = []
  for (const failure of record.failure_expanded || []) {
    const message = (failure.expanded || []).join('\n')
    if (message === '') continue
    events.push({
      name: 'exception',
      attributes: { 'exception.message': truncate(message, EXCEPTION_MESSAGE_MAX_LENGTH) },
    })
  }
  return events
}

const describeTest = (record, { line, column } = {}) => {
  const status = STATUSES[record.result]
  const failed = status === 'fail'
  // Cut without a marker, as the server cuts the JSON upload's reason.
  const reason = failed && record.failure_reason ? truncate(record.failure_reason, FAILURE_REASON_MAX_LENGTH, '') : undefined

  return {
    attributes: testAttributes(record, line, column),
    status,
    failureReason: reason,
    exceptionEvents: failed ? exceptionEvents(record) : [],
  }
}

module.exports = { describeTest, truncate }
