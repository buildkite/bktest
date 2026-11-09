import { version } from 'vitest/package.json'
import { randomUUID, randomBytes } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import semver from 'semver'
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
    this._started = new Map();
    this._modules = new Map();
  }

  onInit(ctx) {
    this._start = Date.now()
    this._paths = new Paths({ rootDir: ctx.config.root }, this._testEnv.location_prefix)
    globalThis[Symbol.for('buildkite.vitest.reporter')] = { shutdown: () => this._finish() }
    const settings = resolveSettings({
      otelEnabled: this._otelEnabled, token: this._options?.token,
      testRunner: 'vitest', frameworkVersion: version, runEnv: this._testEnv, tags: this._tags,
    })
    if (!settings) return
    try {
      const expected = fileURLToPath(new URL('./otel.js', import.meta.url))
      const vmPool = (config) => ['vmThreads', 'vmForks'].includes(config.pool ?? ctx.config.pool)
      const disabledReason = (config) => {
        if (config.browser?.enabled) return 'Vitest browser mode sends test.execution spans only.'
        if (vmPool(config)) return 'Vitest VM pools (vmThreads/vmForks) send test.execution spans only: Vitest ends test spans early because its instanceof Promise check does not recognise a cross-realm Promise.'
        return 'Vitest OpenTelemetry children require Vitest 4.0.11+ with experimental.openTelemetry enabled and the collector sdkPath in the root config; sending test.execution spans only.'
      }
      const sdkEnabled = (config) => {
        const otel = config.experimental?.openTelemetry
        const globalOtel = ctx.config.experimental?.openTelemetry
        const sdk = otel?.sdkPath ?? globalOtel?.sdkPath
        return !config.browser?.enabled && (otel?.enabled ?? globalOtel?.enabled) === true && sdk &&
          (sdk.startsWith('file:') ? fileURLToPath(sdk) : path.resolve(config.root, sdk)) === expected
      }
      const native = (config) => sdkEnabled(config) && !vmPool(config)
      this._native = semver.gte(version, '4.0.11') && !!sdkEnabled(ctx.config)
      this._spool = fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-vitest-otel-'))
      this._settings = { ...settings, spool: this._spool, native: this._native,
        seed: randomBytes(16).toString('hex'), ownerPid: process.pid }
      this._session = createSession(this._settings)
      this._sender = createUploads(settings, this._spool)
      publishSettings(this._settings)
      // A hard worker crash can leave Vitest 4.0.11's pool closure unresolved,
      // reaching natural exit without calling the main SDK's shutdown.
      this._beforeExit = () => this._finish()
      process.once('beforeExit', this._beforeExit)
      this._sigterm = () => {
        this._terminated = true
        // Vitest's logger exits 1 ms after SIGTERM; an async drain cannot finish.
        warn('SIGTERM: Vitest exits before uploads can drain; results not yet uploaded are lost. Rerun the suite to recover missing results.')
      }
      process.prependOnceListener('SIGTERM', this._sigterm)
      const projects = ctx.projects?.length ? ctx.projects : [{ config: ctx.config }]
      for (const { config } of projects) {
        config.env = { ...config.env, BUILDKITE_TEST_COLLECTOR_VITEST_PROJECT: JSON.stringify([config.root, config.name || '', !!native(config)]) }
      }
      if (!this._native) warn(disabledReason(ctx.config))
      else {
        this._nativeProjects = new Set(projects.filter(({ config }) => native(config)).map(({ config }) => config.name || ''))
        const disabled = new Map()
        for (const { config } of projects.filter(({ config }) => !native(config))) {
          const reason = disabledReason(config)
          if (!disabled.has(reason)) disabled.set(reason, [])
          disabled.get(reason).push(config.name || config.root)
        }
        for (const [reason, names] of disabled) warn(`Vitest OpenTelemetry children are disabled for projects: ${names.join(', ')}. ${reason}`)
      }
    } catch (error) {
      warn(`TEST RESULTS MISSING: OpenTelemetry could not be configured (${describeError(error)}); check temporary directory permissions/free space and the SDK installation, then rerun (no JSON fallback).`)
      publishSettings(null)
    }
  }

  onTestModuleCollected(module) {
    if (!this._otelEnabled) return
    const previous = this._modules.get(module.id)
    if (!this._collision && previous && (previous.moduleId !== module.moduleId || previous.project !== module.project)) {
      this._collision = true
      warn(`Vitest mixes up results for [${previous.project.name || 'default'}] ${previous.moduleId} and [${module.project.name || 'default'}] ${module.moduleId} because of an upstream ID hash collision. Renaming either file fixes it. The collector keeps traces separate but uploads the results Vitest reports.`)
    }
    this._modules.set(module.id, module)
  }

  // Vitest reports a test ready from inside its run.test span, even if the test
  // later skips itself. When a suite or module is skipped, including by bail,
  // Vitest reports its tests ready afterwards without running them. Typecheck
  // results never reach a worker.
  onTestCaseReady(test) {
    if (!this._nativeProjects?.has(test.project.name) || test.module.meta().typecheck) return
    let parent = test.parent
    while (parent.type !== 'module' && parent.state() !== 'skipped') parent = parent.parent
    if (parent.state() === 'skipped') return
    const { traceId } = testIds(this._settings.seed, test.project.config.root, test.project.name, test.module.moduleId, test.id)
    this._started.set(traceId, test.project.name)
  }

  onTestCaseResult(test) {
    if (!this._session) return
    const ids = testIds(this._settings.seed, test.project.config.root, test.project.name, test.module.moduleId, test.id)
    if (this._recorded.has(ids.traceId)) return
    this._recorded.add(ids.traceId)
    const record = { id: randomUUID(), ...testEngineRecord(test, this._paths.prefixTestPath(test.module.moduleId)) }
    const diagnostic = test.diagnostic()
    const startTime = diagnostic?.startTime ?? this._start
    const timing = { line: test.location?.line, startTime, ids }
    const handle = this._session.startTestSpan(record, timing)
    handle?.span.setAttributes({ 'vitest.test.id': test.id, 'vitest.test.name': test.name,
      'vitest.test.mode': test.options.mode, 'code.column.number': test.location?.column })
    this._session.finishTestSpan(handle, record, { ...timing, endTime: startTime + (diagnostic?.duration || 0) })
  }

  async onTestModuleEnd(testModule) {
    if (!this._session) return
    for (const test of testModule.children.allTests()) this.onTestCaseResult(test)
    await this._session.forceFlush(testModule.moduleId)
    this._sender.collect(testModule.moduleId)
  }

  _finish() {
    if (this._completion) return this._completion
    return this._completion = (async () => {
      try {
        await this._session?.forceFlush('vitest')
        await this._sender?.drain()
        if (this._native && this._runEnded && !this._terminated) this._checkWorkerRoots()
      } finally {
        await this._session?.shutdown()
        if (this._beforeExit) process.off('beforeExit', this._beforeExit)
        if (this._sigterm) process.off('SIGTERM', this._sigterm)
        if (this._spool && !this._terminated) fs.rmSync(this._spool, { recursive: true, force: true })
      }
    })()
  }

  // Children depend on Vitest's experimental span attributes, which any Vitest
  // release can change. Compare what workers recognised with Vitest's results.
  _checkWorkerRoots() {
    const roots = new Set()
    const ready = new Set()
    for (const project of this._nativeProjects) {
      let text
      // A project without a record has no worker that set up the SDK; each that failed has warned.
      try { text = fs.readFileSync(testIds.rootsFile(this._spool, project), 'utf8') } catch { continue }
      ready.add(project)
      for (const id of text.split('\n')) if (id) roots.add(id)
    }
    const unlinked = [...roots].filter((id) => !this._recorded.has(id)).length
    const unrecognised = [...this._started].filter(([id, project]) => ready.has(project) && !roots.has(id)).length
    const changed = `Vitest ${version}'s experimental OpenTelemetry spans may have changed. Check for a collector update or report this Vitest version; test.execution spans were sent.`
    if (unlinked) warn(`Vitest OpenTelemetry children could not be linked to their tests for ${unlinked} test span(s). ${changed}`)
    else if (unrecognised) warn(`Vitest OpenTelemetry children were not recorded for ${unrecognised} test(s) because their test spans were not recognised. ${changed}`)
  }

  // Vitest 3+ exposes test results through the public reporter API. In Vitest 5,
  // JsonReporter no longer calls writeReport, so subclassing it loses uploads.
  async onTestRunEnd(testModules) {
    if (this._otelEnabled) {
      for (const module of testModules) await this.onTestModuleEnd(module)
      this._runEnded = true
      // Native SDK shutdown runs after pool.close(), unlike onClose callbacks
      // (which race pool closure in Vitest 4). Reporter-only runs have no workers
      // committing children, so they can drain immediately.
      if (!this._native) return this._finish()
      return
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
