const { randomUUID } = require('crypto')
const CI = require('../util/ci')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings, publishSettings, fallbackOutcome } = require('../util/otel/config')
const { warn, describeError } = require('../util/otel/warn')
const testEngineRecord = require('./testEngineRecord')
const { createRecords, recordKey, exportCount, removeRecords } = require('./exportRecords')

const jestVersion = (rootDir) => {
  try {
    return require(require.resolve('jest/package.json', { paths: [rootDir] })).version
  } catch {
    return undefined
  }
}

class JestBuildkiteTestEngineReporter {
  constructor(globalConfig, options) {
    this._globalConfig = globalConfig
    this._options = options
    this._testResults = []
    this._testEnv = new CI().env('jest')
    this._tags = options?.tags
    this._paths = new Paths(globalConfig, this._testEnv.location_prefix)
    this._fallback = fallbackOutcome({ token: options?.token })
    const frameworkVersion = jestVersion(globalConfig.rootDir)
    this._otel = resolveSettings({
      otelEnabled: options?.otelEnabled,
      token: options?.token,
      testRunner: 'jest',
      frameworkVersion,
      runEnv: this._testEnv,
      tags: this._tags,
      unsupported: Number(frameworkVersion?.split('.')[0]) < 29 ? `OpenTelemetry submission needs Jest 29 or newer, not ${frameworkVersion}` : undefined,
    })
    if (this._otel) {
      try {
        this._exportRecords = createRecords()
        publishSettings({ ...this._otel, exportRecords: this._exportRecords, fallback: this._fallback })
      } catch (error) {
        warn(`OpenTelemetry span export disabled: could not create a temporary directory (${describeError(error)}); ${this._fallback}.`)
        publishSettings(null)
        this._otel = null
      }
    }
    this._warned = new Set()
    this._exported = new Map()
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

  async onRunComplete() {
    try {
      this._uploadAmbiguousExports()
      if (this._otel && this._testResults.length === 0) return
      await uploadTestResults(this._testEnv, this._tags, this._testResults, this._options)
    } finally {
      this._removeExportRecords()
    }
  }

  _removeExportRecords() {
    if (!this._exportRecords) return
    try {
      removeRecords(this._exportRecords)
    } catch (error) {
      warn(`OpenTelemetry could not remove its temporary directory ${this._exportRecords} (${describeError(error)}).`)
    }
  }

  async onTestResult(test, testResult) {
    if (this._otel) {
      if (this._holdIfExported(test, testResult)) return
      this._warnNotExported(test)
    }
    this._addJson(testResult)
  }

  _addJson(testResult) {
    const prefixedTestPath = this._paths.prefixTestPath(testResult.testFilePath)

    testResult.testResults.forEach((result) => {
      this._testResults.push({
        id: randomUUID(),
        ...testEngineRecord(result, prefixedTestPath),
        history: {
          section: 'top',
          start_at: testResult.perfStats.start,
          end_at: testResult.perfStats.end,
          duration: result.duration / 1000,
        }
      })
    })
  }

  // Projects differing only in testEnvironmentOptions share an export key.
  // Keep their results until every project's export is accounted for.
  _holdIfExported(test, testResult) {
    let key
    try {
      key = recordKey(test.context.config, testResult.testFilePath)
    } catch {
      return false
    }
    if (exportCount(this._exportRecords, key) === 0) return false
    this._exported.set(key, [...(this._exported.get(key) || []), testResult])
    return true
  }

  // When ownership is ambiguous, prefer duplicate results over missing ones.
  _uploadAmbiguousExports() {
    for (const [key, testResults] of this._exported) {
      if (exportCount(this._exportRecords, key) >= testResults.length) continue
      warn(
        `Jest projects with the same configuration apart from testEnvironmentOptions ran ${testResults[0].testFilePath}, ` +
        `and not all of them exported it to OpenTelemetry; uploading their results as JSON as well, so some may appear twice.`
      )
      testResults.forEach((testResult) => this._addJson(testResult))
    }
  }

  _warnNotExported(test) {
    const fallback = this._fallback
    if (process.env.JEST_JASMINE === '1' || /jest-jasmine2/.test(test.context.config.testRunner)) {
      this._warnOnce('runner', `otelEnabled is set, but only the jest-circus test runner reports to OpenTelemetry; ${fallback}.`)
      return
    }
    this._warnOnce(
      'runtime',
      `No OpenTelemetry events were recorded for ${test.path}; use runtime: 'buildkite-test-collector/jest/runtime'; ${fallback}.`
    )
  }

  _warnOnce(key, message) {
    if (this._warned.has(key)) return
    this._warned.add(key)
    warn(message)
  }
}

module.exports = JestBuildkiteTestEngineReporter
