const { randomUUID } = require('crypto')
const fs = require('fs')
const path = require('path')
const CI = require('../util/ci')
const Debug = require('../util/debug')
const uploadTestResults = require('../util/uploadTestResults')
const Paths = require('../util/paths')
const { resolveSettings, publishSettings, fallbackOutcome } = require('../util/otel/config')
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

const jestRunnerDirs = (rootDir) => [rootDir, path.dirname(require.resolve('jest-runner/package.json', { paths: [rootDir] }))]

// A test file's `@jest-environment` docblock overrides the project's
// testEnvironment; jest-runner resolves it the same way just before the file
// runs, so its own modules are used here to land on the same module.
const environmentFor = (test) => {
  const { rootDir, testEnvironment } = test.context.config
  try {
    const dirs = jestRunnerDirs(rootDir)
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
// results from any other environment still have to go up as JSON. jest-runner
// loads the environment through the project's transformer, so a Babel or
// TypeScript environment that works in the worker must be loaded the same
// way here, or it would be counted twice: once as spans, once as JSON.
const isCollectorEnvironment = async (config, environmentPath) => {
  try {
    const { createScriptTransformer } = requireFrom('@jest/transform', jestRunnerDirs(config.rootDir))
    const transformer = await createScriptTransformer(config)
    const mod = await transformer.requireAndTranspileModule(environmentPath)
    return Boolean(mod && (mod.default || mod).buildkiteTestEngine)
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
    // Jest 27 gives a test environment only its own project's config, so the
    // workers learn the rootDir the JSON upload's locations are relative to here.
    if (this._otel) publishSettings({ ...this._otel, rootDir: globalConfig.rootDir })
    this._fallback = fallbackOutcome({ token: options?.token })
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

  async onTestResult(test, testResult) {
    if (this._otel && await this._exportedByEnvironment(test)) return

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
  async _exportedByEnvironment(test) {
    const fallback = this._fallback
    if (!circusRunner(test.context.config)) {
      this._warnOnce('runner', `otelEnabled is set, but only the jest-circus test runner reports to OpenTelemetry; ${fallback}.`)
      return false
    }
    const environmentPath = environmentFor(test)
    if (environmentPath === null) {
      this._warnOnce(test.path, `otelEnabled is set, but the test environment for ${test.path} could not be resolved; ${fallback}.`)
      return false
    }
    if (!this._environments.has(environmentPath)) {
      this._environments.set(environmentPath, isCollectorEnvironment(test.context.config, environmentPath))
    }
    const exportsSpans = await this._environments.get(environmentPath)
    if (!exportsSpans) {
      this._warnOnce(
        environmentPath,
        `otelEnabled is set, but test environment ${JSON.stringify(environmentPath)} is not buildkite-test-collector/jest/environment ` +
        `(or built with its extend()); for test files using it, ${fallback}.`
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
