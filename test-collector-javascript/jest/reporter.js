const { randomUUID } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')
const CI = require('../util/ci')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings, publishSettings } = require('../util/otel/config')
const { warn, describeError } = require('../util/otel/warn')
const testEngineRecord = require('./testEngineRecord')

const jestVersion = (rootDir) => {
  try { return require(require.resolve('jest/package.json', { paths: [rootDir] })).version } catch { return undefined }
}

class JestBuildkiteTestEngineReporter {
  constructor(globalConfig, options) {
    this._globalConfig = globalConfig
    this._options = options
    this._testResults = []
    this._testEnv = new CI().env('jest')
    this._tags = options?.tags
    this._paths = new Paths(globalConfig, this._testEnv.location_prefix)
    this._otelEnabled = options?.otelEnabled === true
    this._hasCredential = [process.env.BUILDKITE_TESTS_OTLP_TOKEN, options?.token, process.env.BUILDKITE_ANALYTICS_TOKEN].some((value) => value?.trim())
    this._missingFiles = 0
    this._missingTests = 0
    const frameworkVersion = jestVersion(globalConfig.rootDir)
    const settings = resolveSettings({
      otelEnabled: this._otelEnabled, token: options?.token,
      testRunner: 'jest', frameworkVersion, runEnv: this._testEnv, tags: this._tags,
      unsupported: Number(frameworkVersion?.split('.')[0]) < 29 ? `OpenTelemetry submission needs Jest 29 or newer, not ${frameworkVersion}` : undefined,
    })
    if (settings) {
      try {
        this._spool = fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-otel-'))
        this._sender = require('../util/otel/uploads').createUploads(settings, this._spool)
        publishSettings({ ...settings, rootDir: globalConfig.rootDir, spool: this._spool })
        this._sigterm = () => {
          const otherListeners = process.listenerCount('SIGTERM') > 0
          this._stopping = true
          this._terminated = true
          this._finish(5000).finally(() => {
            if (otherListeners) return
            if (process.pid === 1) process.exit(143)
            else process.kill(process.pid, 'SIGTERM')
          })
        }
        process.prependOnceListener('SIGTERM', this._sigterm)
      } catch (error) {
        warn(`TEST RESULTS MISSING: OpenTelemetry could not create its local spool (${describeError(error)}); check temporary directory permissions/free space and the SDK installation, then rerun. Results were not uploaded (no JSON fallback).`)
        publishSettings(null)
      }
    }
  }

  onRunStart() {
    if (this._globalConfig.forceExit) {
      console.warn(
        `--forceExit could potentially terminate any ongoing processes that ` +
        `are attempting to send test executions to Buildkite. ` +
        `Have you considered using \`--detectOpenHandles\` to detect ` +
        'async operations that kept running after all tests finished?',
      )
    }
  }

  // Jest calls completion directly on bail. Gate further dispatch while that
  // completion awaits uploads; already running files are not stopped.
  onTestFileStart() {
    if (this._stopping) return new Promise(() => {})
  }

  onTestFileResult(test, testResult, results) {
    if (!this._otelEnabled) return this._addJson(testResult)
    if (testResult.testExecError && !testResult.testResults.length) return
    if (!this._hasCredential) return // The single configuration warning is enough.
    if (!this._sender) {
      this._missingFiles++
      this._missingTests += testResult.testResults.length
      return
    }
    const jasmine = process.env.JEST_JASMINE === '1' || /jest-jasmine2/.test(test.context.config.testRunner)
    const reason = jasmine ? 'jasmine' : test.context.config.runtime ? 'setup failed' : 'no runtime'
    this._sender.collect(testResult.testFilePath, testResult.testResults.length, reason, test.context.config.id)
  }

  // Kept for consumers invoking the legacy reporter callback directly.
  onTestResult(...args) { return this.onTestFileResult(...args) }

  onRunComplete() {
    if (!this._otelEnabled) return uploadTestResults(this._testEnv, this._tags, this._testResults, this._options)
    return this._finish()
  }

  _finish(budget) {
    this._stopping = true
    const drain = this._sender?.drain(budget)
    return this._completion ||= (async () => {
      try { await drain } finally {
        if (this._missingFiles) warn(`TEST RESULTS MISSING: ${this._missingFiles} file(s), ${this._missingTests} test(s) were not uploaded (settings unusable); fix the earlier OpenTelemetry configuration warning and rerun (unknown child count; no JSON fallback).`)
        if (this._sigterm) process.off('SIGTERM', this._sigterm)
        // A terminating run can still have workers committing files. Do not
        // remove their destination while they are alive.
        if (this._spool && !this._terminated) {
          const cleanup = () => {
            try { fs.rmSync(this._spool, { recursive: true, force: true }) } catch (error) {
              warn(`OpenTelemetry could not remove its temporary directory (${describeError(error)}).`)
            }
          }
          if (this._globalConfig.bail) process.once('exit', cleanup)
          else cleanup()
        }
      }
    })()
  }

  _addJson(testResult) {
    const prefixedTestPath = this._paths.prefixTestPath(testResult.testFilePath)
    testResult.testResults.forEach((result) => {
      this._testResults.push({
        id: randomUUID(), ...testEngineRecord(result, prefixedTestPath),
        history: { section: 'top', start_at: testResult.perfStats.start, end_at: testResult.perfStats.end, duration: result.duration / 1000 },
      })
    })
  }
}

module.exports = JestBuildkiteTestEngineReporter
