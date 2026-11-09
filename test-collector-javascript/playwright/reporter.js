const stripAnsi = require('strip-ansi');
const { randomUUID } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')
const CI = require('../util/ci')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings, credentialFor } = require('../util/otel/config')
const { createSession } = require('../util/otel/session')
const { warn, describeError } = require('../util/otel/warn')
const { truncate } = require('../util/otel/describeTest')

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
    this._attempts = new Map();
    this._endedAttempts = new WeakSet();
    this._stepFiles = new Map();
  }

  onBegin(config) {
    if (!this._otelEnabled) return;

    this._hasCredential = Boolean(credentialFor(this._options.token));
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
      this._sigterm = () => {
        const otherListeners = process.listenerCount('SIGTERM') > 0;
        this._terminated = true;
        this._finish(5000).finally(() => {
          if (otherListeners) return;
          if (process.pid === 1) process.exit(143);
          else process.kill(process.pid, 'SIGTERM');
        });
      };
      process.prependOnceListener('SIGTERM', this._sigterm);
    } catch (error) {
      warn(`TEST RESULTS MISSING: OpenTelemetry could not be configured (${describeError(error)}); check temporary directory permissions/free space and the SDK installation, then rerun (no JSON fallback).`);
    }
  }

  async onEnd() {
    if (this._otelEnabled) {
      return this._finish();
    }
    return new Promise(resolve => {
      uploadTestResults(this._testEnv, this._tags, this._testResults, this._options, resolve);
    })
  }

  _finish(budget = 10000) {
    this._deadline = Math.min(this._deadline ?? Infinity, Date.now() + budget);
    // drain() also shortens an already-running drain after SIGTERM.
    if (this._draining) this._sender?.drain(budget);
    return this._completion ||= (async () => {
      const interruptedAt = Date.now();
      try {
        // globalTimeout can deliver onEnd before onTestEnd(interrupted).
        // Give worker teardown a bounded chance to deliver the real result.
        if (this._attempts.size) {
          let timer;
          await new Promise(resolve => {
            this._attemptsFinished = resolve;
            timer = setTimeout(resolve, 1000);
          });
          clearTimeout(timer);
          this._attemptsFinished = null;
          for (const [result, attempt] of this._attempts) {
            this._recordTestEnd(attempt.test, result, interruptedAt);
          }
        }
        for (const { provider } of this._stepFiles.values()) await provider.shutdown();
        this._draining = true;
        await this._sender?.drain(Math.max(0, this._deadline - Date.now()));
      } finally {
        await this._session?.shutdown();
        if (this._sigterm) process.off('SIGTERM', this._sigterm);
        if (this._missingTests) warn(`TEST RESULTS MISSING: ${this._missingTests} Playwright test execution(s) were not recorded; fix the earlier warnings and rerun (no JSON fallback).`);
        if (this._spool && !this._terminated) {
          try { fs.rmSync(this._spool, { recursive: true, force: true }); } catch (error) {
            warn(`OpenTelemetry could not remove its temporary directory (${describeError(error)}).`);
          }
        }
      }
    })();
  }

  onTestBegin(test, result) {
    if (!this._session || !this._sender) return;
    try {
      const record = this.testEngineRecord(test, result);
      record.id = randomUUID();
      const { line, column } = test.location;
      const handle = this._session.startTestSpan(record, { line, column, startTime: result.startTime.getTime() });
      this._attempts.set(result, { test, handle, id: record.id, steps: new Map(), endTime: result.startTime.getTime() });
    } catch (error) {
      warn(`Could not start the Playwright test span: ${describeError(error)}`);
    }
  }

  // Adapted from aergonaut/playwright-opentelemetry-reporter (MIT); see NOTICE.md.
  onStepBegin(test, result, step) {
    const attempt = this._attempts.get(result);
    if (!attempt?.handle) return;
    attempt.endTime = Math.max(attempt.endTime, step.startTime.getTime());
    try {
      let file = this._stepFiles.get(test.location.file);
      if (!file) {
        const forwarder = this._session.childExport().createForwarder();
        const provider = this._session.createChildProvider([{
          onStart: span => forwarder.onStart(span),
          onEnd: span => forwarder.onEnd(span),
          forceFlush: async () => {},
          shutdown: () => forwarder.shutdown(),
        }]);
        file = { provider, forwarder, tracer: provider.getTracer('buildkite-test-collector/playwright', require('../package.json').version) };
        this._stepFiles.set(test.location.file, file);
      }
      file.forwarder.addTrace(attempt.handle.span.spanContext().traceId);
      const { api } = this._session;
      const parent = step.parent ? attempt.steps.get(step.parent) : attempt.handle.span;
      const context = api.trace.setSpan(api.ROOT_CONTEXT, parent);
      // Playwright interpolates values without escaping embedded quotes. Redact
      // the whole quoted region, including locator literals, to avoid fragments.
      // Older versions also render unquoted API arguments in parentheses.
      const title = step.category === 'pw:api'
        ? step.title.replace(/["'`][\s\S]*["'`]/g, '"[REDACTED]"').replace(/\([\s\S]*\)/g, '([REDACTED])')
        : step.title;
      const span = file.tracer.startSpan(title, {
        startTime: step.startTime,
        attributes: {
          'test.step.name': title,
          'test.step.category': step.category,
          ...(step.location && {
            'code.file.path': this._paths.prefixTestPath(step.location.file),
            'code.line.number': step.location.line,
            'code.column.number': step.location.column,
          }),
        },
      }, context);
      attempt.steps.set(step, span);
    } catch (error) {
      warn(`OpenTelemetry could not record a Playwright step (${describeError(error)}); test results are unaffected.`);
    }
  }

  onStepEnd(test, result, step) {
    const attempt = this._attempts.get(result);
    if (!attempt) return;
    const endTime = step.startTime.getTime() + step.duration;
    attempt.endTime = Math.max(attempt.endTime, endTime);
    const steps = attempt.steps;
    const span = steps?.get(step);
    if (!span) return;
    if (step.error) {
      span.setStatus({ code: this._session.api.SpanStatusCode.ERROR, message: truncate(stripAnsi(step.error.message || ''), 10243) });
      span.addEvent('exception', { 'exception.message': truncate(stripAnsi(step.error.stack || step.error.message || ''), 10243) }, endTime);
    }
    span.end(endTime);
    steps.delete(step);
  }

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

    this._recordTestEnd(test, testResult);
  }

  _recordTestEnd(test, testResult, interruptedAt) {
    // Late worker callbacks must not duplicate attempts closed at the deadline.
    if (this._endedAttempts.has(testResult)) return;
    this._endedAttempts.add(testResult);
    try {
      const result = interruptedAt === undefined ? testResult : {
        ...testResult, status: 'interrupted', duration: Math.max(0, interruptedAt - testResult.startTime.getTime()),
      };
      const record = this.testEngineRecord(test, result);
      const attempt = this._attempts.get(testResult);
      record.id = attempt?.id || randomUUID();
      // Cancellation is not a test failure; JSON retains its historical 'unknown'.
      if (result.status === 'interrupted') record.result = 'skipped';
      const { line, column } = test.location;
      const startTime = testResult.startTime.getTime();
      const handle = attempt?.handle || this._session.startTestSpan(record, { line, column, startTime });
      // Playwright's duration excludes time outside selected timeout slots.
      const endTime = Math.max(startTime + result.duration, attempt?.endTime ?? startTime);
      // A worker can stop without sending onStepEnd for its open steps.
      for (const span of attempt?.steps.values() || []) {
        span.setAttribute('test.step.incomplete', true);
        span.end(endTime);
      }
      this._session.finishTestSpan(handle, record, { line, column, endTime });
      if (handle) {
        this._session.forceFlush(test.location.file);
        this._sender.collect(test.location.file, 1, 'setup failed');
      }
    } catch (error) {
      this._missingTests += 1;
      warn(`TEST RESULTS MISSING: Could not report the Playwright test execution over OpenTelemetry: ${describeError(error)}`);
    } finally {
      this._attempts.delete(testResult);
      if (!this._attempts.size) this._attemptsFinished?.();
    }
  }

  testEngineRecord(test, testResult) {
    const scope = test.titlePath().join(' ');
    const fileName = this._paths.prefixTestPath(test.location.file);
    const location = [fileName, test.location.line, test.location.column].join(':');

    const filteredTags = (test.tags || []).filter(tag => this._tagPattern.test(tag));
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
