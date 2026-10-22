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

// Limits count Unicode code points, as the server and the other collectors
// do, so a name full of emoji keeps the identity of its JSON upload; the SDK's
// own UTF-16 limits are set wide enough never to cut these values again.
const truncate = (text, max, omission = '...') => {
  if (text.length <= max) return text
  const codePoints = Array.from(text)
  if (codePoints.length <= max) return text
  return `${codePoints.slice(0, max - omission.length).join('')}${omission}`
}

// The SDK skips undefined attribute values, so a missing line is left as is.
const testAttributes = (record, line) => ({
  'buildkite.test.scope': record.scope,
  'buildkite.test.name': record.name,
  'test.case.name': [record.scope, record.name].filter(Boolean).join(' '),
  'test.suite.name': record.scope,
  'code.file.path': record.file_name,
  'code.line.number': Number.isInteger(line) ? line : undefined,
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

const describeTest = (record, { line } = {}) => {
  const status = STATUSES[record.result]
  const failed = status === 'fail'
  // Cut without a marker, as the server cuts the JSON upload's reason.
  const reason = failed && record.failure_reason ? truncate(record.failure_reason, FAILURE_REASON_MAX_LENGTH, '') : undefined

  return {
    attributes: testAttributes(record, line),
    status,
    failureReason: reason,
    exceptionEvents: failed ? exceptionEvents(record) : [],
  }
}

module.exports = { describeTest, truncate }
