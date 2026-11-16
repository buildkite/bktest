const { createHash } = require('crypto')

// The reporter publishes the run's seed here before workers start, so the
// app-tracing fixture can name each test.execution span as a parent.
const RUN_ENV = 'BUILDKITE_TEST_COLLECTOR_PLAYWRIGHT_OTEL'

// Playwright's test ID already includes the project and repeat index.
const testIds = (seed, testId, retry) => {
  const hash = createHash('sha256').update(JSON.stringify([seed, testId, retry])).digest('hex')
  return { traceId: hash.slice(0, 32), spanId: hash.slice(32, 48) }
}

const publishRun = (run) => { process.env[RUN_ENV] = JSON.stringify(run) }

const currentRun = () => {
  try {
    const run = JSON.parse(process.env[RUN_ENV] || 'null')
    return typeof run?.seed === 'string' ? run : null
  } catch {
    return null
  }
}

module.exports = { testIds, publishRun, currentRun }
