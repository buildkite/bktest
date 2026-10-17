const { randomUUID } = require('crypto')
const fs = require('fs')
const path = require('path')
const CI = require('../util/ci')
const Debug = require('../util/debug')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings } = require('../util/otel/config')
const { warn, describeError } = require('../util/otel/warn')
const testEngineRecord = require('./testEngineRecord')

const requireFrom = (request, dirs) => require(require.resolve(request, { paths: dirs }))

const jestVersion = (rootDir) => {
  try {
    return requireFrom('jest/package.json', [rootDir]).version
  } catch {
    return undefined
  }
}

// A test file's `@jest-environment` docblock overrides the project's
// testEnvironment; jest-runner resolves it the same way just before the file
// runs, so its own modules are used here to land on the same module.
const environmentFor = (test) => {
  const { rootDir, testEnvironment } = test.context.config
  try {
    const dirs = [rootDir, path.dirname(require.resolve('jest-runner/package.json', { paths: [rootDir] }))]
    const docblock = requireFrom('jest-docblock', dirs)
    const pragma = docblock.parse(docblock.extract(fs.readFileSync(test.path, 'utf8')))['jest-environment']
    if (typeof pragma !== 'string') return testEnvironment
    return requireFrom('jest-resolve', dirs).resolveTestEnvironment({
      rootDir,
      testEnvironment: pragma,
      requireResolveFunction: require.resolve,
    })
  } catch (error) {
    Debug.log(`Could not resolve the test environment for ${test.path}: ${describeError(error)}`)
    return null
  }
}

// Workers export spans only when the file ran under our test environment, so
// results from any other environment still have to go up as JSON.
const isCollectorEnvironment = (environmentPath) => {
  try {
    const mod = require(environmentPath)
    return Boolean((mod.default || mod).buildkiteTestEngine)
  } catch (error) {
    Debug.log(`Could not load test environment ${environmentPath}: ${describeError(error)}`)
    return false
  }
}

const circusRunner = (config) => process.env.JEST_JASMINE !== '1' && !/jest-jasmine2/.test(config.testRunner)

class JestBuildkiteTestEngineReporter {
  constructor(globalConfig, options) {
    this._globalConfig = globalConfig
    this._options = options
    this._testResults = []
    this._testEnv = (new CI()).env('jest');
    this._tags = options?.tags;
    this._paths = new Paths(globalConfig, this._testEnv.location_prefix)
    this._otel = resolveSettings({
      otelEnabled: options?.otelEnabled,
      token: options?.token,
      testRunner: 'jest',
      frameworkVersion: jestVersion(globalConfig.rootDir),
      runEnv: this._testEnv,
      tags: this._tags,
    })
    this._environments = new Map()
    this._warned = new Set()
  }

  onRunStart(results, options) {
    if (this._globalConfig.forceExit) {
      console.warn(
        `--forceExit could potentially terminate any ongoing processes that ` +
        `are attempting to send test executions to Buildkite. ` +
        `Have you considered using \`--detectOpenHandles\` to detect ` +
        'async operations that kept running after all tests finished?',
      );
    }
  }

  onRunComplete(_test, _results, _options) {
    if (this._otel && this._testResults.length === 0) return
    return uploadTestResults(this._testEnv, this._tags, this._testResults, this._options)
  }

  onTestStart(test) {
  }

  onTestResult(test, testResult) {
    if (this._otel && this._exportedByEnvironment(test)) return

    const prefixedTestPath = this._paths.prefixTestPath(testResult.testFilePath);

    testResult.testResults.forEach((result) => {
      this._testResults.push({
        'id': randomUUID(),
        ...testEngineRecord(result, prefixedTestPath),
        'history': {
          'section': 'top',
          'start_at': testResult.perfStats.start,
          'end_at': testResult.perfStats.end,
          'duration': result.duration / 1000,
        }
      })
    })
  }

  // Only jest-circus calls the environment's handleTestEvent, so files run by
  // jest-jasmine2 cannot export either.
  _exportedByEnvironment(test) {
    if (!circusRunner(test.context.config)) {
      this._warnOnce('runner', 'otelEnabled is set, but only the jest-circus test runner reports to OpenTelemetry; results will be uploaded as JSON instead.')
      return false
    }
    const environmentPath = environmentFor(test)
    if (environmentPath === null) {
      this._warnOnce(test.path, `otelEnabled is set, but the test environment for ${test.path} could not be resolved; its results will be uploaded as JSON instead.`)
      return false
    }
    if (!this._environments.has(environmentPath)) {
      this._environments.set(environmentPath, isCollectorEnvironment(environmentPath))
    }
    const exportsSpans = this._environments.get(environmentPath)
    if (!exportsSpans) {
      this._warnOnce(
        environmentPath,
        `otelEnabled is set, but test environment ${JSON.stringify(environmentPath)} is not buildkite-test-collector/jest/environment ` +
        '(or built with its extend()); results from test files using it will be uploaded as JSON instead.'
      )
    }
    return exportsSpans
  }

  _warnOnce(key, message) {
    if (this._warned.has(key)) return
    this._warned.add(key)
    warn(message)
  }
}

module.exports = JestBuildkiteTestEngineReporter
