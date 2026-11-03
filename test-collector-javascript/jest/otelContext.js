const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')
const { workerFor } = require('./otelWorker')
const JestOtelPackages = require('./otelPackages')
const { loadInstrumentations } = require('./otelInstrumentations')

const disabledMessage = (error) => `OpenTelemetry child span export disabled: ${describeError(error).split('\n')[0]}; test.execution export remains enabled`
const SUITE_PROVIDER = 'OpenTelemetry child spans are not exported because the suite registers its own tracer provider; ' +
  'to get them, let the collector provide the provider under Jest. test.execution export remains enabled.'

// One async context manager per worker, used by the sandbox (for the tests'
// API) and the environment's realm (for instrumentation). It must be the same
// instance in both so that a test's context is visible to instrumentation in
// either realm.
let sharedContextManager = null
const contextManager = () => {
  if (!sharedContextManager) {
    const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
    sharedContextManager = new AsyncLocalStorageContextManager().enable()
  }
  return sharedContextManager
}

const PROBE = Symbol.for('buildkite.test.execution.probe')
const hasContextManager = (api) => {
  const probe = api.ROOT_CONTEXT.setValue(PROBE, true)
  return api.context.with(probe, () => api.context.active().getValue(PROBE)) === true
}

// Registered in the environment's realm, where no suite code runs.
const ensureContextManager = (api) => {
  if (hasContextManager(api)) return
  if (!api.context.setGlobalContextManager(contextManager())) throw new Error('could not register an async context manager')
}

// The sandbox never gets a registered context manager either, which would
// refuse the suite's own. Each copy of the API falls back to a context manager
// that does nothing when none is registered, and the collector makes that one
// use its own. One the suite registers takes over until it disables it.
const fillContextManager = (api) => {
  if (hasContextManager(api)) return
  const fallback = api.context._getContextManager()
  if (fallback.constructor.name === 'NoopContextManager') {
    const manager = contextManager()
    for (const method of ['active', 'with', 'bind']) fallback[method] = manager[method].bind(manager)
  }
  if (!hasContextManager(api)) throw new Error('could not set up an async context manager')
}

// The collector never registers a tracer provider in the sandbox, which
// would refuse the suite's own registration. It becomes the delegate of each
// copy of the API's proxy provider that has none, which serves both tracers
// obtained earlier and later getTracer() calls; a provider the suite
// registers replaces it. Returns those proxies, or null when the suite has a
// provider of its own.
const emptyProxies = (apis, ours) => {
  const empty = []
  for (const api of apis) {
    const proxy = api.trace.getTracerProvider()
    const delegate = proxy.getDelegate()
    if (delegate === new api.ProxyTracerProvider().getDelegate()) empty.push(proxy)
    else if (delegate !== ours) return null
  }
  return empty
}

// Connects one test file's sandbox to OpenTelemetry when the collector's Jest
// runtime is set. Nothing is decided until its first test or beforeEach hook
// runs, after setup files, the file's own top-level code and beforeAll hooks.
// A provider the suite registers by then is never replaced or reconfigured,
// and the file exports test spans only; one it registers later takes over
// from the collector's until the suite removes it.
class JestOtelContext {
  constructor({ session, settings, testPath, rootDir }) {
    this.session = session
    this.testPath = testPath
    this.rootDir = rootDir
    this.mode = null
    this.runtime = null
    this.traces = new Set()
    this.apis = new Set()
    this.cleanups = []
    // Conditions every worker would hit are reported once per run.
    this.warnOnce = (key, message) => warnOncePerRun(settings.exportRecords, key, message)
  }

  // Called by the collector's Jest runtime when it is created for this file.
  attachRuntime(runtime) {
    this.runtime = runtime
    try {
      this.instrumentations = loadInstrumentations(this.rootDir, this.warnOnce)
    } catch (error) {
      warn(`OpenTelemetry instrumentation could not be loaded (${describeError(error)}); test.execution export remains enabled.`)
    }
    this.packages = new JestOtelPackages(this, this.instrumentations || [])
  }

  _worker() {
    if (!this.worker) this.worker = workerFor(this.session, (this.instrumentations || []).map((entry) => entry.instance))
    return this.worker
  }

  load(file, load) {
    // A suite-owned provider must not activate collector package patches.
    if (!emptyProxies([...this.apis, this.session.api], this.provider)) return load()
    return this.packages.load(file, load)
  }

  // Called by the collector's Jest runtime for each copy of the API the file
  // requires, of which jest.resetModules() and jest.isolateModules() make
  // more. A copy loaded during a test records children at once.
  loadedApi(api) {
    if (this.apis.has(api)) return
    try {
      if (typeof api.trace.getTracerProvider().setDelegate !== 'function') return
    } catch {
      return // a mocked API
    }
    this.apis.add(api)
    if (this.mode === 'managed') this._fill()
  }

  // Test bodies and hooks are only wrapped when children are possible.
  get canExportChildren() {
    return Boolean(this.runtime)
  }

  setup() {
    if (this.mode !== null) return
    this.mode = 'off'
    if (!this.runtime) return
    try {
      this.api = this.runtime.requireModule(this.testPath, '@opentelemetry/api')
      this.loadedApi(this.api)
      // Also a provider registered outside the sandbox, for example with
      // NODE_OPTIONS=--require: instrumentation there sees every file's tests.
      if (!emptyProxies([...this.apis, this.session.api])) {
        this.warnOnce('suite-provider', SUITE_PROVIDER)
        return
      }
      ensureContextManager(this.session.api)
      // As OpenTelemetry's SDK setup does, so that an in-process server
      // handling a test's request continues the test's trace. Instrumentation
      // propagates in the environment's realm, so the sandbox's propagator is
      // left to the suite.
      if (this.session.api.propagation.fields().length === 0) this.session.api.propagation.setGlobalPropagator(this.session.childPropagator())
      this.forwarder = this.session.childExport().createForwarder()
      this.provider = this._worker().tracerProvider
      this.mode = 'managed'
      for (const traceId of this.traces) this.forwarder.addTrace(traceId)
    } catch (error) {
      this._cleanUp()
      this.mode = 'off'
      this.warnOnce(`setup:${describeError(error)}`, disabledMessage(error))
    }
  }

  // Before each test body and per-test hook, and for a copy of the API loaded
  // during one: fills in what the sandbox lacks for children, which the suite
  // may have reset since, without replacing anything it registered. Returns
  // false while the suite has a provider.
  _fill() {
    try {
      const empty = emptyProxies(this.apis, this.provider)
      if (!empty) {
        this.worker.close(this)
        this.warnOnce('suite-provider', SUITE_PROVIDER)
        return false
      }
      for (const api of this.apis) fillContextManager(api)
      for (const proxy of empty) proxy.setDelegate(this.provider)
      this.worker.open(this)
      return true
    } catch (error) {
      this.warnOnce(`setup:${describeError(error)}`, disabledMessage(error))
      return false
    }
  }

  // Each test span's trace is the set of spans the forwarder accepts.
  began(started) {
    const traceId = started?.handle?.span?.spanContext?.().traceId
    if (!traceId || !this.runtime) return
    this.traces.add(traceId)
    if (this.mode === 'managed') this.forwarder.addTrace(traceId)
  }

  // Runs a test body or per-test hook with its test span current, in the
  // sandbox's API and the environment's, so instrumentation in either realm
  // parents its spans under the test and the forwarder recognises them.
  // Setup runs here, inside test code, rather than at test_start: Jest 30.5
  // refuses to load modules between tests, and setup loads the OpenTelemetry
  // API and SDK into the sandbox.
  run(started, fn, receiver, args) {
    this.setup()
    if (this.mode !== 'managed' || !started?.handle || !this._fill()) return fn.apply(receiver, args)
    const { span } = started.handle
    const contexts = [...new Set([this.api, this.session.api])].map((api) => [api, api.trace.setSpan(api.context.active(), span)])
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

  // Close the worker's gate before destroying the sandbox. Children that
  // already started keep forwarding when they end. Only sandbox package
  // instrumentation is disabled here.
  async teardown() {
    this.mode = 'done'
    this.worker?.close(this)
    if (this.forwarder) await this.forwarder.shutdown()
    this._cleanUp()
  }
}

module.exports = JestOtelContext
