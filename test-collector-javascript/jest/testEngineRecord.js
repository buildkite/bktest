// JSON and OpenTelemetry must derive the same test identity.
const RESULTS = {
  passed: 'passed',
  pending: 'pending',
  failed: 'failed',
  todo: 'skipped'
}

const testEngineRecord = (testResult, prefixedTestPath) => {
  const messages = testResult.status === 'failed'
    ? testResult.failureMessages.join(' ').replace(/\u001b[^m]*?m/g, '').split('\n')
    : []
  return {
    scope: testResult.ancestorTitles.join(' '),
    name: testResult.title,
    location: testResult.location ? `${prefixedTestPath}:${testResult.location.line}` : null,
    file_name: prefixedTestPath,
    result: RESULTS[testResult.status],
    failure_reason: messages[0],
    failure_expanded: [{ expanded: messages.slice(1) }],
  }
}

module.exports = testEngineRecord
