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
const { createRecords, isExported, removeRecords } = require('./exportRecords')

const jestVersion = (rootDir) => {
  try {
    return require(require.resolve('jest/package.json', { paths: [rootDir] })).version
  } catch {
    return undefined
  }
}

// Loads modules the way the project's jest-runner does, from its own
// dependencies; under Yarn Plug'n'Play the project cannot load them itself.
const runnerRequire = (config) => createRequire(config.runner)

// A test file's `@jest-environment` docblock overrides the project's
// testEnvironment; jest-runner resolves it the same way just before the file
// runs, so its own modules are used here to land on the same module.
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

// Only explains a JSON fallback; whether a file was exported comes from its
// worker's record. jest-runner loads the environment through the project's
// transformer, so a Babel or TypeScript environment is loaded the same way.
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

const circusRunner = (config) => process.env.JEST_JASMINE !== '1' && !/jest-jasmine2/.test(config.testRunner)

class JestBuildkiteTestEngineReporter {
  constructor(globalConfig, options) {
    this._globalConfig = globalConfig
    this._options = options
    this._testResults = []
    this._testEnv = (new CI()).env('jest');
    this._tags = options?.tags;
    this._paths = new Paths(globalConfig, this._testEnv.location_prefix)
    this._fallback = fallbackOutcome({ token: options?.token })
    this._otel = resolveSettings({
      otelEnabled: options?.otelEnabled,
      token: options?.token,
      testRunner: 'jest',
      frameworkVersion: jestVersion(globalConfig.rootDir),
      runEnv: this._testEnv,
      tags: this._tags,
    })
    if (this._otel) {
      try {
        this._exportRecords = createRecords()
        // Jest 27 gives a test environment only its own project's config, so the
        // workers learn the rootDir the JSON upload's locations are relative to here.
        publishSettings({ ...this._otel, rootDir: globalConfig.rootDir, exportRecords: this._exportRecords, fallback: this._fallback })
      } catch (error) {
        warn(`OpenTelemetry span export disabled: could not create a temporary directory (${describeError(error)}); ${this._fallback}.`)
        publishSettings(null)
        this._otel = null
      }
    }
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
    if (this._exportRecords) removeRecords(this._exportRecords)
    if (this._otel && this._testResults.length === 0) return
    return uploadTestResults(this._testEnv, this._tags, this._testResults, this._options)
  }

  onTestStart(test) {
  }

  async onTestResult(test, testResult) {
    if (this._otel) {
      if (isExported(this._exportRecords, test.context.config.id, testResult.testFilePath)) return
      await this._warnNotExported(test)
    }

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

  // Says why a file was not exported. Only jest-circus calls the environment's
  // handleTestEvent, so files run by jest-jasmine2 cannot export; a file whose
  // collector environment could not set up OTel was explained by its worker.
  async _warnNotExported(test) {
    const fallback = this._fallback
    if (!circusRunner(test.context.config)) {
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
