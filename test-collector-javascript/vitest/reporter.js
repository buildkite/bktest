import { version } from 'vitest/package.json'
import { randomUUID, randomBytes } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import CI from '../util/ci.js'
import uploadTestResults from '../util/uploadTestResults.js'
import Paths from '../util/paths'
import { resolveSettings, publishSettings } from '../util/otel/config.js'
import { createSession } from '../util/otel/session.js'
import { createUploads } from '../util/otel/uploads.js'
import { warn, describeError } from '../util/otel/warn.js'
import testEngineRecord from './testEngineRecord.js'
import testIds from './otelIds.js'

class VitestBuildkiteTestEngineReporter {
  constructor(options) {
    this._options = options;
    this._testEnv = new CI().env('vitest');
    this._tags = options?.tags;
    this._otelEnabled = options?.otelEnabled === true;
    this._recorded = new Set();
  }

  onInit(ctx) {
    this._start = Date.now()
    this._paths = new Paths({ rootDir: ctx.config.root }, this._testEnv.location_prefix)
    globalThis[Symbol.for('buildkite.vitest.reporter')] = true
    const settings = resolveSettings({
      otelEnabled: this._otelEnabled, token: this._options?.token,
      testRunner: 'vitest', frameworkVersion: version, runEnv: this._testEnv, tags: this._tags,
    })
    if (!settings) return
    try {
      this._spool = fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-vitest-otel-'))
      this._settings = { ...settings, spool: this._spool, rootDir: ctx.config.root,
        seed: randomBytes(16).toString('hex'), ownerPid: process.pid }
      this._session = createSession(this._settings)
      this._sender = createUploads(settings, this._spool)
      publishSettings(this._settings)
      this._sigterm = () => {
        const otherListeners = process.listenerCount('SIGTERM') > 0
        this._terminated = true
        this._finish(5000).finally(() => {
          if (otherListeners) return
          if (process.pid === 1) process.exit(143)
          else process.kill(process.pid, 'SIGTERM')
        })
      }
      process.prependOnceListener('SIGTERM', this._sigterm)
      const expected = fileURLToPath(new URL('./otel.js', import.meta.url))
      const projects = ctx.projects?.length ? ctx.projects : [{ config: ctx.config }]
      if (projects.some(({ config }) => {
        const otel = config.experimental?.openTelemetry
        const sdk = otel?.sdkPath
        return config.browser?.enabled || !otel?.enabled || !sdk ||
          (sdk.startsWith('file:') ? fileURLToPath(sdk) : path.resolve(config.root, sdk)) !== expected
      })) warn('Vitest OpenTelemetry children are disabled for projects without the collector sdkPath, including Vitest 3, browser mode and user-owned SDKs; sending test.execution spans only. Use Vitest 4.0.11+ with experimental.openTelemetry enabled and sdkPath pointing to buildkite-test-collector/vitest/otel.js for children.')
    } catch (error) {
      warn(`TEST RESULTS MISSING: OpenTelemetry could not be configured (${describeError(error)}); check temporary directory permissions/free space and the SDK installation, then rerun (no JSON fallback).`)
      publishSettings(null)
    }
  }

  onTestCaseResult(test) {
    if (!this._session || this._recorded.has(test.id)) return
    this._recorded.add(test.id)
    const record = { id: randomUUID(), ...testEngineRecord(test, this._paths.prefixTestPath(test.module.moduleId)) }
    const diagnostic = test.diagnostic()
    const startTime = diagnostic?.startTime ?? this._start
    const timing = { line: test.location?.line, column: test.location?.column, startTime,
      ids: testIds(this._settings.seed, test.id) }
    const handle = this._session.startTestSpan(record, timing)
    handle?.span.setAttributes({ 'vitest.test.id': test.id, 'vitest.test.name': test.name, 'vitest.test.mode': test.options.mode })
    this._session.finishTestSpan(handle, record, { ...timing, endTime: startTime + (diagnostic?.duration || 0) })
  }

  async onTestModuleEnd(testModule) {
    if (!this._session) return
    for (const test of testModule.children.allTests()) this.onTestCaseResult(test)
    await this._session.forceFlush(testModule.moduleId)
    this._sender.collect(testModule.moduleId)
  }

  _finish(budget) {
    // A SIGTERM can shorten a drain already in progress.
    if (this._completion) { this._sender?.drain(budget); return this._completion }
    return this._completion = (async () => {
      try {
        await this._session?.forceFlush('vitest')
        await this._sender?.drain(budget)
      } finally {
        await this._session?.shutdown()
        if (this._sigterm) process.off('SIGTERM', this._sigterm)
        if (this._spool && !this._terminated) fs.rmSync(this._spool, { recursive: true, force: true })
      }
    })()
  }

  // Vitest 3+ exposes test results through the public reporter API. In Vitest 5,
  // JsonReporter no longer calls writeReport, so subclassing it loses uploads.
  async onTestRunEnd(testModules) {
    if (this._otelEnabled) {
      for (const module of testModules) await this.onTestModuleEnd(module)
      return this._finish()
    }
    const originStart = this._start;
    const testResults = testModules.flatMap((testModule) => {
      const prefixedTestPath = this._paths.prefixTestPath(testModule.moduleId);
      const tests = Array.from(testModule.children.allTests());
      const { startTime, endTime } = this.fileTimings(tests, originStart);

      return tests.map((test) => {
        return {
          id: randomUUID(),
          ...testEngineRecord(test, prefixedTestPath),
          history: {
            section: 'top',
            start_at: (startTime - originStart) / 1000,
            end_at: (endTime - originStart) / 1000,
            // Preserve null in JSON for tests with no execution timing.
            duration: test.diagnostic()?.duration / 1000,
          },
        };
      });
    });

    return uploadTestResults(
      this._testEnv,
      this._tags,
      testResults,
      this._options,
    );
  }

  // Match the JSON reporter's file-wide window, falling back to the run start
  // when no tests in the file executed.
  fileTimings(tests, originStart) {
    let startTime = Number.POSITIVE_INFINITY;
    let endTime = 0;

    for (const test of tests) {
      const diagnostic = test.diagnostic();
      if (!diagnostic) continue;
      startTime = Math.min(startTime, diagnostic.startTime);
      endTime = Math.max(endTime, diagnostic.startTime + diagnostic.duration);
    }

    if (startTime === Number.POSITIVE_INFINITY) startTime = originStart;
    return { startTime, endTime: Math.max(endTime, startTime) };
  }

}

// Freeze the original implementation for Vitest 1–2. Its removed import must
// never be loaded on Vitest 5.
export default Number(version.split('.')[0]) < 3
  ? (await import('./legacy-reporter.js')).default
  : VitestBuildkiteTestEngineReporter;
