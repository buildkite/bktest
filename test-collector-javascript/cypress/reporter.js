const { randomUUID } = require('crypto')
const MochaBuildkiteTestEngineReporter = require('../mocha/reporter')
const failureExpanded = require('../util/failureExpanded')
const { isOtelEnabled, resolveSettings } = require('../util/otel/config')
const { sessionFor } = require('../util/otel/session')
const { warn, describeError, setWarningHandler } = require('../util/otel/warn')

// Cypress recreates the reporter per spec, but keeps this module loaded.
// undefined = not decided yet; null = JSON for the rest of this process.
let session

// Cypress uses mocha under the hood for assertions, so there is some overlap
// between this reporter and the mocha reporter. They differ in how they
// report errors.
class CypressBuildkiteTestEngineReporter extends MochaBuildkiteTestEngineReporter {
  constructor(runner, options) {
    super(runner, options, 'cypress')
    if (!isOtelEnabled(options.reporterOptions.otelEnabled)) return

    if (session === undefined) {
      session = null
      // Cypress filters Electron stderr, including console.warn, by default.
      setWarningHandler((message) => console.log(message))
      resolveSettings({
        otelEnabled: true,
        token: this._options.token,
        testRunner: 'cypress',
        runEnv: this._testEnv,
        tags: this._tags,
        probe: (settings) => { session = sessionFor(settings) },
      })
      if (session) process.once('beforeExit', () => session.shutdown())
    }
    this._otel = session
    if (this._otel) {
      this._runner = runner
      this._reported = new Set()
      // Unlike Mocha's test end, this includes pending tests and each retry,
      // with the final verdict after hooks and Cypress's wall-clock timing.
      runner.on('test:after:run', (test) => this.exportTest(test))
    }
  }

  testFinished(test) {
    if (this._otel) return
    this._testResults.push(this.testEngineRecord(test))
  }

  exportTest(test) {
    this._reported.add(test)
    try {
      // Interrupted runs can leave tests active/processing, or without a state.
      const result = test.state == null || ['processing', 'active'].includes(test.state) ? 'skipped' : test.state
      if (!['passed', 'failed', 'pending', 'skipped'].includes(result)) {
        warn(`TEST RESULTS MISSING: Unknown Cypress test state ${JSON.stringify(test.state)}; test execution was not exported.`)
        return
      }
      const record = this.testEngineRecord(test)
      record.id = randomUUID()
      // The inherited Mocha mapping drops Cypress's pending/skipped states.
      record.result = result
      const startedAt = new Date(test.wallClockStartedAt).getTime()
      const startTime = startedAt > 0 ? startedAt : Date.now()
      const duration = Number.isFinite(test.wallClockDuration) ? Math.max(0, test.wallClockDuration) : 0
      const handle = this._otel.startTestSpan(record, { startTime })
      this._otel.finishTestSpan(handle, record, { endTime: startTime + duration })
    } catch (error) {
      this._otel.metrics.recordFailures++
      warn(`TEST RESULTS MISSING: Could not report a Cypress test to OpenTelemetry: ${describeError(error)}`)
    }
  }

  async done(failures, exit) {
    if (!this._otel) return super.done(failures, exit)
    try {
      // A failed before hook leaves later tests in the tree without events.
      this._runner.suite.eachTest((test) => {
        if (!this._reported.has(test)) this.exportTest(test)
      })
      await this._otel.forceFlush()
    } catch (error) {
      warn(`TEST RESULTS MISSING: Could not finish Cypress OpenTelemetry reporting: ${describeError(error)}`)
    } finally {
      exit(failures)
    }
  }

  testEngineRecord(test) {
    const prefixedTestPath = this._paths.prefixTestPath(this.getRootParentFile(test))

    const { failureReason, failureExpanded } = this.getFailureReason(test.err)

    return {
      'id': test.testEngineId,
      'name': test.title,
      'scope': this.scope(test),
      'identifier': test.fullTitle(),
      'file_name': prefixedTestPath,
      'location': prefixedTestPath,
      'result': this.testEngineResult(test.state),
      'failure_reason': failureReason,
      'failure_expanded': failureExpanded,
      'history': {
        'section': 'top',
        'start_at': test.startAt,
        'end_at': performance.now() / 1000,
        'duration': test.duration / 1000,
      }
    }
  }

  getFailureReason(err) {
    if (err === undefined) {
      return {
        failureReason: undefined,
        failureExpanded: [],
      }
    }

    return {
      failureReason: err.message,
      failureExpanded: failureExpanded([err]),
    }
  }
}

module.exports = CypressBuildkiteTestEngineReporter
