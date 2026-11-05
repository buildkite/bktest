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
    const frameworkVersion = jestVersion(globalConfig.rootDir)
    const settings = resolveSettings({
      otelEnabled: this._otelEnabled, token: options?.token,
      testRunner: 'jest', frameworkVersion, runEnv: this._testEnv, tags: this._tags,
      unsupported: Number(frameworkVersion?.split('.')[0]) < 29 ? `OpenTelemetry submission needs Jest 29 or newer, not ${frameworkVersion}` : undefined,
    })
    if (settings) {
      try {
        this._spool = fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-otel-'))
        this._sender = require('../util/otel/exporter').createSender(settings, this._spool)
        publishSettings({ ...settings, rootDir: globalConfig.rootDir, spool: this._spool })
        this._sigterm = () => {
          this._stopping = true
          this._terminated = true
          this._finish().finally(() => {
            process.removeAllListeners('SIGTERM')
            process.kill(process.pid, 'SIGTERM')
          })
        }
        process.once('SIGTERM', this._sigterm)
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

  // The public start hook is awaited before dispatch. Gate new files as soon
  // as bail is reached; otherwise Jest keeps scheduling while completion awaits
  // HTTP. Already running files are not stopped. Jest owns the eventual exit.
  onTestFileStart() {
    if (this._stopping) return new Promise(() => {})
  }

  onTestFileResult(test, testResult, results) {
    if (!this._otelEnabled) return this._addJson(testResult)
    if (this._globalConfig.bail && results?.numFailedTests >= this._globalConfig.bail &&
      !this._globalConfig.watch && !this._globalConfig.watchAll) this._stopping = true
    if (this._completion) return // Late arrivals are counted outside the fixed drain snapshot.
    if (!this._sender) {
      warn(`TEST RESULTS MISSING: ${testResult.testResults.length} test.execution span(s), unknown child count for ${test.path} were not uploaded; fix the earlier OpenTelemetry configuration warning and rerun (no JSON fallback).`)
      return
    }
    if (!this._sender.collect(testResult.testFilePath)) {
      const runner = process.env.JEST_JASMINE === '1' || /jest-jasmine2/.test(test.context.config.testRunner)
      const fix = runner ? 'use the jest-circus test runner' : "set runtime to 'buildkite-test-collector/jest/runtime' and see earlier worker warnings"
      warn(`TEST RESULTS MISSING: ${testResult.testResults.length} test.execution span(s), unknown child count for ${test.path} were not uploaded; ${fix}, then rerun (no JSON fallback).`)
    }
  }

  // Kept for consumers invoking the legacy reporter callback directly.
  onTestResult(...args) { return this.onTestFileResult(...args) }

  onRunComplete() {
    if (!this._otelEnabled) return uploadTestResults(this._testEnv, this._tags, this._testResults, this._options)
    return this._finish()
  }

  _finish() {
    return this._completion ||= (async () => {
      try { await this._sender?.drain() } finally {
        if (this._sigterm) process.off('SIGTERM', this._sigterm)
        // A terminating run can still have workers committing files. Do not
        // remove their destination while they are alive.
        if (this._spool && !this._terminated) {
          const cleanup = () => {
            try { fs.rmSync(this._spool, { recursive: true, force: true }) } catch (error) {
              warn(`OpenTelemetry could not remove its temporary directory (${describeError(error)}).`)
            }
          }
          if (this._stopping) process.once('exit', cleanup)
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
