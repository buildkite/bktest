const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const semver = require('semver')
const { TEST_CONTEXT } = require('../util/otel/children')
const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')
const { isBuiltin, createInstrumentation, enabledInstrumentations, moduleDefinitions } = require('../util/otel/instrumentations')
const { workerFor } = require('./otelWorker')

const NOOP_PROCESSOR = { onStart() {}, onEnd() {}, forceFlush: async () => {}, shutdown: async () => {} }
const RUNTIME_HINT = "set runtime: 'buildkite-test-collector/jest/runtime' in the Jest config (Jest 27: moduleLoader)"

const disabledMessage = (error) => `OpenTelemetry child span export disabled: ${describeError(error).split('\n')[0]}; test.execution export remains enabled`

// Instrumentation packages are loaded once per worker in the environment's
// realm, from the project's root. Those that patch Node's built-in modules or
// listen on diagnostics channels are used from here, in the realm of the
// objects they wrap, and enabled once per worker. Those that patch packages
// are created again in each test file's sandbox: Jest gives every file its
// own copies of packages and of intrinsics such as Promise, which
// instrumentation compares against (`result instanceof Promise` in
// @opentelemetry/instrumentation-pg).
const instrumentationSets = new Map()
const instrumentationSet = (entries, rootDir, warnOnce) => {
  const key = JSON.stringify([rootDir, entries])
  if (instrumentationSets.has(key)) return instrumentationSets.get(key)
  const requireFromProject = createRequire(path.join(rootDir, 'package.json'))
  const set = { shared: [], sandboxed: [], packages: new Set() }
  for (const entry of enabledInstrumentations(entries)) {
    try {
      const instance = createInstrumentation(requireFromProject, entry)
      const names = moduleDefinitions(instance).map((definition) => definition.name)
      const packages = names.filter((name) => !isBuiltin(name))
      if (packages.length < names.length || names.length === 0) set.shared.push({ name: entry.name, instance })
      if (packages.length > 0) {
        set.sandboxed.push({ ...entry, packages: new Set(packages) })
        for (const name of packages) set.packages.add(name)
      }
    } catch (error) {
      warnOnce(`load:${entry.name}`, `OpenTelemetry instrumentation ${entry.name} could not be loaded (${describeError(error).split('\n')[0]}); continuing without it. test.execution export remains enabled.`)
    }
  }
  instrumentationSets.set(key, set)
  return set
}

const hasTracerProvider = (api) => {
  const provider = api.trace.getTracerProvider()
  return !(provider instanceof api.ProxyTracerProvider) || provider.getDelegate() !== new api.ProxyTracerProvider().getDelegate()
}

// What require-in-the-middle tells instrumentation about a module, worked out
// from the file Jest loaded: its package, the package's root and version, and
// whether the file is the package's main module.
const moduleDetails = (file) => {
  const segments = file.split(path.sep)
  const index = segments.lastIndexOf('node_modules')
  if (index === -1) return null
  const width = segments[index + 1]?.startsWith('@') ? 2 : 1
  if (segments.length <= index + width + 1) return null
  return {
    name: segments.slice(index + 1, index + 1 + width).join('/'),
    basedir: segments.slice(0, index + 1 + width).join(path.sep),
    relative: segments.slice(index + 1 + width).join(path.sep),
  }
}

const packageVersions = new Map()
const packageVersion = (basedir) => {
  if (!packageVersions.has(basedir)) {
    let version
    try {
      version = JSON.parse(fs.readFileSync(path.join(basedir, 'package.json'), 'utf8')).version
    } catch {
      version = undefined
    }
    packageVersions.set(basedir, version)
  }
  return packageVersions.get(basedir)
}

const packageOf = (specifier) => specifier.split('/').slice(0, specifier.startsWith('@') ? 2 : 1).join('/')
const isPath = (specifier) => specifier.startsWith('.') || path.isAbsolute(specifier)

// Whether a require could load a module of an instrumented package, without
// resolving it.
const mayMatch = (from, moduleName, packages) => {
  if (!isPath(moduleName)) return packages.has(packageOf(moduleName))
  return !moduleName.startsWith('.') || packages.has(moduleDetails(from)?.name)
}

const describeModule = (from, moduleName, packages) => {
  let file
  try {
    file = isPath(moduleName)
      ? require.resolve(path.resolve(path.dirname(from), moduleName))
      : require.resolve(moduleName, { paths: [path.dirname(from)] })
  } catch {
    return null
  }
  const details = moduleDetails(file)
  if (!details || !packages.has(details.name)) return null
  let main = false
  try {
    main = require.resolve(details.name, { paths: [details.basedir] }) === file
  } catch {
    main = false
  }
  return {
    path: file,
    name: details.name,
    file: main ? details.name : path.join(details.name, details.relative),
    main,
    version: packageVersion(details.basedir),
  }
}

// Some instrumentation (undici) also checks its config on every operation.
// The sandbox's copy only patches the package modules the runtime hands it:
// built-in modules belong to the worker's shared copy.
const enable = (instance, tracerProvider) => {
  instance.setTracerProvider(tracerProvider)
  instance.setConfig({ ...instance.getConfig(), enabled: true })
  instance.enable()
}

// @opentelemetry/instrumentation's own rule for whether a definition applies.
const supports = (ranges, version, includePrerelease) => (version === undefined
  ? ranges.includes('*')
  : ranges.some((range) => semver.satisfies(version, range, { includePrerelease })))

const patchModule = (definition, record) => {
  let { exports } = record
  if (record.main) {
    if (typeof definition.patch === 'function' && supports(definition.supportedVersions, record.version, definition.includePrerelease)) {
      exports = definition.patch(exports, record.version) ?? exports
    }
    return exports
  }
  for (const file of definition.files || []) {
    if (path.normalize(file.name) === path.normalize(record.file) && supports(file.supportedVersions, record.version, definition.includePrerelease)) {
      exports = file.patch(exports, record.version) ?? exports
    }
  }
  return exports
}

// Connects one test file's sandbox to OpenTelemetry. Nothing is decided until
// its first test or beforeEach hook runs, after setup files, the file's own
// top-level code and beforeAll hooks, so a provider the suite registers wins:
// the collector then never replaces or reconfigures it, and its
// instrumentation records nothing for the file.
class JestOtelContext {
  constructor({ session, settings, testPath, rootDir }) {
    this.session = session
    this.entries = settings.instrumentations || []
    this.testPath = testPath
    this.rootDir = rootDir
    this.mode = null
    this.runtime = null
    this.bridgeApi = null
    this.worker = null
    this.resolved = new Map()
    this.loading = new Set()
    this.loads = new WeakMap()
    this.records = []
    this.traces = new Set()
    this.sandboxed = new Map()
    this.cleanups = []
    // Conditions every worker would hit are reported once per run.
    this.warnOnce = (key, message) => warnOncePerRun(settings.exportRecords, key, message)
  }

  // Called by the collector's Jest runtime when it is created for this file,
  // before setup files and the test file load. The worker's instrumentation
  // is enabled from here, unless OpenTelemetry is configured for the whole
  // process: instrumentation that listens on diagnostics channels must be
  // listening when an app is built (@fastify/otel hooks each Fastify
  // instance as it initializes). If the worker cannot record children,
  // setup reports why.
  attachRuntime(runtime) {
    this.runtime = runtime
    try {
      this.instrumentations = this.entries.length ? instrumentationSet(this.entries, this.rootDir, this.warnOnce) : null
    } catch (error) {
      warn(`OpenTelemetry instrumentation could not be loaded (${describeError(error)}); test.execution export remains enabled.`)
    }
    if (!this.instrumentations || hasTracerProvider(this.session.api)) return
    try {
      this._worker()
    } catch {
      // reported by setup
    }
  }

  // Test bodies and hooks are only wrapped when children are possible.
  get canExportChildren() {
    return Boolean(this.runtime || this.bridgeApi)
  }

  // createBuildkiteSpanProcessor() from buildkite-test-collector/jest/otel:
  // SDK 2.x providers take processors only in their constructor.
  createSpanProcessor(sandboxApi) {
    try {
      if (sandboxApi && !this.bridgeApi) this.bridgeApi = sandboxApi
      if (!this.forwarder) this.forwarder = this.session.childExport().createForwarder()
      return this.forwarder
    } catch (error) {
      this.warnOnce(`processor:${describeError(error)}`, disabledMessage(error))
      return NOOP_PROCESSOR
    }
  }

  // Called by the runtime for every module a test file's code requires, with
  // a function that loads it. Modules of packages that sandboxed
  // instrumentation patches are patched as they load, before the file's
  // provider is decided, so that what an app sets up as it loads (Express
  // routes and middleware) is instrumented too. The patches record nothing
  // unless the collector manages the file. As in require-in-the-middle, each
  // file is patched under its own name, once per evaluation and only once it
  // has finished loading: a package's main module can export an internal
  // file's exports unchanged (Express 4's index.js), and a circular require
  // returns a module that is still loading (node-redis's pool.js requires its
  // unfinished index.js).
  load(from, moduleName, load) {
    const module = this._instrumented(from, moduleName)
    if (!module || this.loading.has(module.path)) return load()
    this.loading.add(module.path)
    let exports
    try {
      exports = load()
    } finally {
      this.loading.delete(module.path)
    }
    if (exports === null || (typeof exports !== 'object' && typeof exports !== 'function')) return exports
    let byPath = this.loads.get(exports)
    if (!byPath) {
      byPath = new Map()
      this.loads.set(exports, byPath)
    }
    let record = byPath.get(module.path)
    if (!record) {
      record = { ...module, exports, patchedBy: new Set() }
      byPath.set(module.path, record)
      this.records.push(record)
      this._patch(record)
    }
    return record.exports
  }

  // The module a require loads, when it belongs to an instrumented package.
  _instrumented(from, moduleName) {
    const packages = this.instrumentations?.packages
    if (!packages?.size || (this.mode !== null && this.mode !== 'managed')) return null
    if (!moduleName || isBuiltin(moduleName) || !mayMatch(from, moduleName, packages)) return null
    const key = `${from}\0${moduleName}`
    if (!this.resolved.has(key)) {
      let module = null
      try {
        module = describeModule(from, moduleName, packages)
      } catch {
        module = null
      }
      this.resolved.set(key, module)
    }
    return this.resolved.get(key)
  }

  setup() {
    if (this.mode !== null) return
    this.mode = 'off'
    let api
    try {
      api = this.runtime ? this.runtime.requireModule(this.testPath, '@opentelemetry/api') : this.bridgeApi
    } catch (error) {
      if (this.entries.length) this.warnOnce('api', disabledMessage(error))
      return
    }
    if (!api) {
      if (this.entries.length) this.warnOnce('runtime', `otelInstrumentations needs the collector's Jest runtime: ${RUNTIME_HINT}. Exporting test.execution spans only.`)
      return
    }
    this.api = api
    try {
      if (hasTracerProvider(api) || hasTracerProvider(this.session.api)) {
        this._useSuiteProvider(api)
      } else if (this.runtime) {
        this._useCollectorProvider(api)
      } else if (this.entries.length) {
        this.warnOnce('runtime', `otelInstrumentations needs the collector's Jest runtime: ${RUNTIME_HINT}. Exporting test.execution spans only.`)
      }
    } catch (error) {
      if (this.worker) this.worker.close(this)
      this._cleanUp()
      this.mode = 'off'
      this.warnOnce(`setup:${describeError(error)}`, disabledMessage(error))
    }
  }

  _useSuiteProvider(api) {
    if (!this.forwarder) {
      const outside = !hasTracerProvider(api)
      this.warnOnce(outside ? 'outside-provider' : 'suite-provider', outside
        ? 'OpenTelemetry child spans are not exported: OpenTelemetry is configured outside the Jest test sandbox. test.execution export remains enabled.'
        : 'OpenTelemetry child spans are not exported: the suite registers its own tracer provider, so add createBuildkiteSpanProcessor() ' +
          "from 'buildkite-test-collector/jest/otel' to its spanProcessors. test.execution export remains enabled.")
      return
    }
    this.mode = 'suite'
    for (const traceId of this.traces) this.forwarder.addTrace(traceId)
  }

  // The worker's child provider records for this file until its teardown.
  _useCollectorProvider(api) {
    const worker = this._worker()
    this.forwarder = this.session.childExport().createForwarder()
    worker.open(this, api)
    this.mode = 'managed'
    for (const traceId of this.traces) this.forwarder.addTrace(traceId)
    for (const record of this.records) this._patch(record)
  }

  // Before setup, a provider already registered settles that the suite owns
  // this file, so nothing is patched for it.
  _ownedElsewhere() {
    if (hasTracerProvider(this.session.api)) return true
    try {
      return hasTracerProvider(this.runtime.requireModule(this.testPath, '@opentelemetry/api'))
    } catch {
      return false
    }
  }

  _patch(record) {
    if (this.mode === null && this._ownedElsewhere()) return
    for (const entry of this.instrumentations.sandboxed) {
      if (!entry.packages.has(record.name) || record.patchedBy.has(entry.name)) continue
      record.patchedBy.add(entry.name)
      const sandboxed = this._sandboxed(entry)
      if (!sandboxed) continue
      try {
        for (const definition of sandboxed.definitions) {
          if (definition.name === record.name && entry.packages.has(definition.name)) record.exports = patchModule(definition, record)
        }
      } catch (error) {
        this.warnOnce(`patch:${entry.name}:${record.file}`, `OpenTelemetry instrumentation ${entry.name} could not patch ${record.file} (${describeError(error)}); test.execution export remains enabled.`)
      }
    }
  }

  _worker() {
    if (!this.worker) this.worker = workerFor(this.session, this.instrumentations?.shared || [])
    return this.worker
  }

  // The instrumentation's sandbox copy, created on first use in this file.
  // If the worker cannot record children, setup reports why.
  _sandboxed(entry) {
    if (this.sandboxed.has(entry.name)) return this.sandboxed.get(entry.name)
    let sandboxed = null
    let tracerProvider
    try {
      tracerProvider = this._worker().tracerProvider
    } catch {
      this.sandboxed.set(entry.name, null)
      return null
    }
    try {
      const instance = createInstrumentation((name) => this.runtime.requireModule(this.testPath, name), entry)
      // disable() undoes what enable() set up outside the sandbox, such as
      // diagnostics channel subscriptions.
      this.cleanups.push(() => instance.disable())
      enable(instance, tracerProvider)
      sandboxed = { instance, definitions: moduleDefinitions(instance) }
    } catch (error) {
      this.warnOnce(`sandbox:${entry.name}`, `OpenTelemetry instrumentation ${entry.name} could not be enabled (${describeError(error)}); continuing without it. test.execution export remains enabled.`)
    }
    this.sandboxed.set(entry.name, sandboxed)
    return sandboxed
  }

  // Each test span's trace is the set of spans the forwarder accepts.
  // Without the collector's runtime nothing is loaded into the sandbox, so
  // the decision can be made as the first test starts.
  began(started) {
    const traceId = started?.handle?.span?.spanContext?.().traceId
    if (!traceId) return
    this.traces.add(traceId)
    if (!this.runtime) this.setup()
    if (this.mode === 'managed' || this.mode === 'suite') this.forwarder.addTrace(traceId)
  }

  // Runs a test body or per-test hook with its test span current, in the
  // sandbox's API and the environment's, so instrumentation in either realm
  // parents its spans under the test and the forwarder recognises them.
  // Setup runs here, inside test code, rather than at test_start: Jest 30.5
  // refuses to load modules between tests, and setup loads the OpenTelemetry
  // API, the SDK and sandboxed instrumentation into the sandbox.
  run(started, fn, receiver, args) {
    this.setup()
    if ((this.mode !== 'managed' && this.mode !== 'suite') || !started?.handle) return fn.apply(receiver, args)
    const { span } = started.handle
    const traceId = span.spanContext().traceId
    const contexts = [...new Set([this.api, this.session.api])].map((api) => [api, api.trace.setSpan(api.context.active(), span).setValue(TEST_CONTEXT, traceId)])
    const enter = (index) => (index === contexts.length
      ? fn.apply(receiver, args)
      : contexts[index][0].context.with(contexts[index][1], () => enter(index + 1)))
    const result = enter(0)
    if (fn.constructor?.name === 'GeneratorFunction' && result) {
      for (const method of ['next', 'throw', 'return']) {
        let bound = result[method].bind(result)
        for (const [api, context] of contexts) bound = api.context.bind(context, bound)
        result[method] = bound
      }
    }
    return result
  }

  _cleanUp() {
    for (const cleanup of this.cleanups.reverse()) {
      try {
        cleanup()
      } catch (error) {
        warn(`Could not unpatch OpenTelemetry instrumentation: ${describeError(error)}`)
      }
    }
    this.cleanups = []
  }

  // Before Jest destroys the sandbox: no more recording, forwarding or
  // patching for this file.
  async teardown() {
    this.mode = 'done'
    if (this.worker) this.worker.close(this)
    if (this.forwarder) await this.forwarder.shutdown()
    this._cleanUp()
  }
}

module.exports = JestOtelContext
