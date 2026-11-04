const { randomUUID } = require('crypto')
const fs = require('fs')
const { createRequire } = require('module')
const CI = require('../util/ci')
const Debug = require('../util/debug')
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

// Yarn Plug'n'Play exposes Jest's dependencies to the runner, not the project.
const runnerRequire = (config) => createRequire(config.runner)

const environmentFor = (test) => {
  const { rootDir, testEnvironment } = test.context.config
  try {
    const runner = runnerRequire(test.context.config)
    const docblock = runner('jest-docblock')
    const pragma = docblock.parse(docblock.extract(fs.readFileSync(test.path, 'utf8')))['jest-environment']
    if (typeof pragma !== 'string') return testEnvironment
    return runner('jest-resolve').resolveTestEnvironment({
      rootDir,
      testEnvironment: pragma,
      requireResolveFunction: runner.resolve,
    })
  } catch (error) {
    Debug.log(`Could not resolve the test environment for ${test.path}: ${describeError(error)}`)
    return null
  }
}

// Match jest-runner's loading of Babel and TypeScript environments.
const isCollectorEnvironment = async (config, environmentPath) => {
  try {
    const { createScriptTransformer } = runnerRequire(config)('@jest/transform')
    const transformer = await createScriptTransformer(config)
    const mod = await transformer.requireAndTranspileModule(environmentPath)
    return Boolean(mod && (mod.default || mod).buildkiteTestEngine)
  } catch (error) {
    Debug.log(`Could not load test environment ${environmentPath}: ${describeError(error)}`)
    return false
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
    this._environments = new Map()
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
      await this._warnNotExported(test)
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

  async _warnNotExported(test) {
    const fallback = this._fallback
    if (process.env.JEST_JASMINE === '1' || /jest-jasmine2/.test(test.context.config.testRunner)) {
      this._warnOnce('runner', `otelEnabled is set, but only the jest-circus test runner reports to OpenTelemetry; ${fallback}.`)
      return
    }
    const environmentPath = environmentFor(test)
    if (environmentPath === null) {
      this._warnOnce(test.path, `otelEnabled is set, but the test environment for ${test.path} could not be resolved; ${fallback}.`)
      return
    }
    if (!this._environments.has(environmentPath)) {
      this._environments.set(environmentPath, isCollectorEnvironment(test.context.config, environmentPath))
    }
    if (await this._environments.get(environmentPath)) return
    this._warnOnce(
      environmentPath,
      `otelEnabled is set, but test environment ${JSON.stringify(environmentPath)} is not buildkite-test-collector/jest/environment ` +
      `(or built with its extend()); for test files using it, ${fallback}.`
    )
  }

  _warnOnce(key, message) {
    if (this._warned.has(key)) return
    this._warned.add(key)
    warn(message)
  }
}

module.exports = JestBuildkiteTestEngineReporter
