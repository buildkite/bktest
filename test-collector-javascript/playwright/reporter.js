const stripAnsi = require('strip-ansi');
const { randomUUID } = require('crypto')
const CI = require('../util/ci')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings } = require('../util/otel/config')
const { sessionFor } = require('../util/otel/session')
const { warn, describeError } = require('../util/otel/warn')

/**
 * JSDoc Imports
 *
 * @typedef {import('@playwright/test/reporter').FullConfig} FullConfig
 * @typedef {import('@playwright/test/reporter').FullResult} FullResult
 * @typedef {import('@playwright/test/reporter').Reporter} Reporter
 * @typedef {import('@playwright/test/reporter').Suite} Suite
 * @typedef {import('@playwright/test/reporter').TestCase} TestCase
 * @typedef {import('@playwright/test/reporter').TestResult} TestResult
 */

/**
 * A playwright reporter that uploads test results to Buildkite Test Engine
 *
 * @implements {import('@playwright/test/reporter').Reporter}
 */
class PlaywrightBuildkiteTestEngineReporter {

  constructor(options) {
    this._testResults = [];
    this._testEnv = (new CI()).env('playwright');
    this._tags = options?.tags;
    this._options = options;
    this._paths = new Paths({ cwd: process.cwd() }, this._testEnv.location_prefix);
    this._tagPattern = /^@[^:]+:[^:]+$/;
    this._session = null;
  }

  onBegin(config) {
    if (!this._options?.otelEnabled) return;

    resolveSettings({
      otelEnabled: true,
      token: this._options.token,
      testRunner: 'playwright',
      frameworkVersion: config.version,
      runEnv: this._testEnv,
      tags: this._tags,
      probe: settings => { this._session = sessionFor(settings); },
    });
  }

  async onEnd() {
    if (this._session) {
      await this._session.forceFlush();
      await this._session.shutdown();
      return;
    }
    return new Promise(resolve => {
      uploadTestResults(this._testEnv, this._tags, this._testResults, this._options, resolve);
    })
  }

  onTestBegin() { }

  /**
   *
   * @param {TestCase} test
   * @param {TestResult} testResult
   */
  onTestEnd(test, testResult) {
    if (!this._session) {
      this._testResults.push(this.testEngineRecord(test, testResult));
      return;
    }

    try {
      const record = this.testEngineRecord(test, testResult);
      record.id = randomUUID();
      // JSON retains its historical 'unknown'; OTLP would discard that status.
      if (testResult.status === 'interrupted') record.result = 'failed';
      const { line, column } = test.location;
      const startTime = testResult.startTime.getTime();
      // Worker timings include hooks and exclude reporter/transport latency.
      const handle = this._session.startTestSpan(record, { line, column, startTime });
      this._session.finishTestSpan(handle, record, { line, column, endTime: startTime + testResult.duration });
    } catch (error) {
      warn(`TEST RESULTS MISSING: Could not report the Playwright test execution over OpenTelemetry: ${describeError(error)}`);
    }
  }

  testEngineRecord(test, testResult) {
    const scope = test.titlePath().join(' ');
    const fileName = this._paths.prefixTestPath(test.location.file);
    const location = [fileName, test.location.line, test.location.column].join(':');

    const filteredTags = test.tags.filter(tag => this._tagPattern.test(tag));
    const tags = filteredTags.reduce((acc, test) => {
      const [key, value] = test.slice(1).split(':');
      acc[key] = value;

      return acc;
    }, {});

    return {
      'id': test.id,
      'name': test.title,
      'scope': scope,
      'location': location,
      'file_name': fileName,
      'result': this.testEngineResult(testResult.status),
      'failure_reason': this.testEngineFailureReason(testResult),
      'failure_expanded': this.testEngineFailureExpanded(testResult),
      'history': {
        'section': 'top',
        'start_at': testResult.startTime.getTime(),
        'duration': testResult.duration / 1000,
      },
      'tags': tags
    };
  }

  testEngineResult(status) {
    // Playwright test statuses:
    // - failed
    // - interrupted
    // - passed
    // - skipped
    // - timedOut
    //
    // Buildkite Test Engine execution results:
    // - failed
    // - passed
    // - pending
    // - skipped
    // - unknown
    return {
      failed: 'failed',
      interrupted: 'unknown',
      passed: 'passed',
      skipped: 'skipped',
      timedOut: 'failed',
    }[status]
  }

  /**
   *
   * @param {TestResult} testResult
   */
  testEngineFailureReason(testResult) {
    if (testResult.error == undefined) return "";

    const reason = stripAnsi(testResult.error.message).split("\n")[0];

    return reason;
  }

  /**
   *
   * @param {TestResult} testResult
   */
  testEngineFailureExpanded(testResult) {
    let expandedErrors = [];

    if (testResult.errors) {
      for (const error of testResult.errors) {
        if (error.stack) {
          const stack = stripAnsi(error.stack).split("\n");
          const snippet = stripAnsi(error.snippet)?.split("\n") || [];
          expandedErrors = expandedErrors.concat(stack, snippet);
        } else if (error.message) {
          const message = stripAnsi(error.message).split("\n");
          expandedErrors = expandedErrors.concat(message);
        }
      }
    }

    return [
      {
        expanded: expandedErrors
      }
    ];
  }
}

module.exports = PlaywrightBuildkiteTestEngineReporter
