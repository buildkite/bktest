// Maps one Jest assertion result to the fields Test Engine identifies an
// execution by. Both the JSON reporter and the OTel environment go through
// here so the two upload paths derive the same test identity.

// Jest test statuses: passed, pending, failed, todo
// Test Engine results: passed, failed, pending, skipped, unknown
const RESULTS = {
  passed: 'passed',
  pending: 'pending',
  failed: 'failed',
  todo: 'skipped'
}

const failureMessages = (testResult) => {
  if (testResult.status !== 'failed') return []

  // Strip ANSI color codes from messages and split each line
  return testResult.failureMessages.join(' ').replace(/\u001b[^m]*?m/g,'').split("\n")
}

const testEngineRecord = (testResult, prefixedTestPath) => {
  const messages = failureMessages(testResult)

  return {
    'scope': testResult.ancestorTitles.join(' '),
    'name': testResult.title,
    'location': testResult.location ? `${prefixedTestPath}:${testResult.location.line}` : null,
    'file_name': prefixedTestPath,
    'result': RESULTS[testResult.status],
    'failure_reason': messages[0],
    'failure_expanded': [
      {
        expanded: messages.slice(1)
      }
    ]
  }
}

module.exports = testEngineRecord
