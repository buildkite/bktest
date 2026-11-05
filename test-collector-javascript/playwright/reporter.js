const stripAnsi = require('strip-ansi');
const { randomUUID } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')
const CI = require('../util/ci')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings } = require('../util/otel/config')
const { createSession } = require('../util/otel/session')
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
    const enabled = options?.otelEnabled;
    this._otelEnabled = enabled === true || (typeof enabled === 'string' && enabled.trim().toLowerCase() === 'true');
    this._missingTests = 0;
  }

  onBegin(config) {
    if (!this._otelEnabled) return;

    this._hasCredential = [process.env.BUILDKITE_TESTS_OTLP_TOKEN, this._options.token, process.env.BUILDKITE_ANALYTICS_TOKEN].some(value => value?.trim());
    const settings = resolveSettings({
      otelEnabled: true,
      token: this._options.token,
      testRunner: 'playwright',
      frameworkVersion: config.version,
      runEnv: this._testEnv,
      tags: this._tags,
    });
    if (!settings) return;
    try {
      this._spool = fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-playwright-otel-'));
      this._session = createSession({ ...settings, spool: this._spool });
      this._sender = require('../util/otel/uploads').createUploads(settings, this._spool);
    } catch (error) {
      warn(`TEST RESULTS MISSING: OpenTelemetry could not be configured (${describeError(error)}); check temporary directory permissions/free space and the SDK installation, then rerun (no JSON fallback).`);
    }
  }

  async onEnd() {
    if (this._otelEnabled) {
      try {
        await this._sender?.drain();
      } finally {
        await this._session?.shutdown();
        if (this._missingTests) warn(`TEST RESULTS MISSING: ${this._missingTests} Playwright test execution(s) were not recorded; fix the earlier warnings and rerun (no JSON fallback).`);
        if (this._spool) {
          try { fs.rmSync(this._spool, { recursive: true, force: true }); } catch (error) {
            warn(`OpenTelemetry could not remove its temporary directory (${describeError(error)}).`);
          }
        }
      }
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
    if (!this._otelEnabled) {
      this._testResults.push(this.testEngineRecord(test, testResult));
      return;
    }
    if (!this._hasCredential) return;
    if (!this._sender) {
      this._missingTests += 1;
      return;
    }

    try {
      const record = this.testEngineRecord(test, testResult);
      record.id = randomUUID();
      // Cancellation is not a test failure; JSON retains its historical 'unknown'.
      if (testResult.status === 'interrupted') record.result = 'skipped';
      const { line, column } = test.location;
      const startTime = testResult.startTime.getTime();
      // Worker timings include hooks and exclude reporter/transport latency.
      const handle = this._session.startTestSpan(record, { line, column, startTime });
      this._session.finishTestSpan(handle, record, { line, column, endTime: startTime + testResult.duration });
      if (handle) {
        this._session.forceFlush(test.location.file);
        this._sender.collect(test.location.file, 1, 'setup failed');
      }
    } catch (error) {
      this._missingTests += 1;
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
