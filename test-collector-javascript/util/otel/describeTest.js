// Takes the same record the JSON path uploads, so both paths derive the same
// Test Engine identity.

const FAILURE_REASON_MAX_LENGTH = 1024
const EXCEPTION_MESSAGE_MAX_LENGTH = 10243
const EXCEPTION_STACKTRACE_MAX_LENGTH = 10243

// A result not listed here leaves the span's 'unset' placeholder, which the
// server skips.
const STATUSES = {
  passed: 'pass',
  failed: 'fail',
  pending: 'skipped',
  skipped: 'skipped',
}

// Limits count Unicode code points, as the server and the other collectors
// do, so a name full of emoji keeps the identity of its JSON upload; the SDK's
// own UTF-16 limits are set wide enough never to cut these values again.
const truncate = (text, max) => {
  if (text.length <= max) return text
  const codePoints = Array.from(text)
  if (codePoints.length <= max) return text
  return `${codePoints.slice(0, max - 3).join('')}...`
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
    const stacktrace = (failure.backtrace || []).join('\n')
    if (message === '' && stacktrace === '') continue
    const attributes = {}
    if (message !== '') attributes['exception.message'] = truncate(message, EXCEPTION_MESSAGE_MAX_LENGTH)
    if (stacktrace !== '') attributes['exception.stacktrace'] = truncate(stacktrace, EXCEPTION_STACKTRACE_MAX_LENGTH)
    events.push({
      name: 'exception',
      attributes,
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

module.exports = { describeTest, truncate }
