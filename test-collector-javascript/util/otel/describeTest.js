// Takes the same record the JSON path uploads, so both paths derive the same
// Test Engine identity.

const FAILURE_REASON_MAX_LENGTH = 1024
const EXCEPTION_MESSAGE_MAX_LENGTH = 10243

// A result not listed here leaves the span's 'unset' placeholder, which the
// server skips.
const STATUSES = {
  passed: 'pass',
  failed: 'fail',
  pending: 'skipped',
  skipped: 'skipped',
}

// Limits are in UTF-16 code units, like the SDK's own attribute limits; the
// cut never leaves half a surrogate pair behind.
const truncate = (text, max) => {
  if (text.length <= max) return text
  let head = text.slice(0, max - 3)
  if (/[\uD800-\uDBFF]$/.test(head)) head = head.slice(0, -1)
  return `${head}...`
}

// The SDK skips undefined attribute values, so a missing line is left as is.
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
  const reason = failed && record.failure_reason ? truncate(record.failure_reason, FAILURE_REASON_MAX_LENGTH) : undefined

  return {
    attributes: testAttributes(record, line, column),
    status,
    failureReason: reason,
    exceptionEvents: failed ? exceptionEvents(record) : [],
  }
}

module.exports = { describeTest }
