const path = require('path')
const { createRequire } = require('module')
const { TEST_CONTEXT } = require('../util/otel/children')
const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')
const { isBuiltin, createInstrumentation, enabledInstrumentations, patchedModules } = require('../util/otel/instrumentations')

const NOOP_PROCESSOR = { onStart() {}, onEnd() {}, forceFlush: async () => {}, shutdown: async () => {} }
const RUNTIME_HINT = "set runtime: 'buildkite-test-collector/jest/runtime' in the Jest config (Jest 27: moduleLoader)"

const disabledMessage = (error) => `OpenTelemetry child span export disabled: ${describeError(error).split('\n')[0]}; test.execution export remains enabled`

// Instrumentation packages are loaded once per worker in the environment's
// realm, from the project's root, and used from there, in the realm of the
// built-in modules they patch. Only instrumentation that patches nothing but
// Node's built-in modules, or only listens on diagnostics channels (undici),
// is used. Instrumentation that patches packages is skipped: Jest loads a
// test file's packages itself, bypassing the require hooks it relies on.
const instrumentationSets = new Map()
const instrumentationSet = (entries, rootDir, warnOnce) => {
  const key = JSON.stringify([rootDir, entries])
  if (instrumentationSets.has(key)) return instrumentationSets.get(key)
  const requireFromProject = createRequire(path.join(rootDir, 'package.json'))
  const instances = []
  for (const entry of enabledInstrumentations(entries)) {
    let instance
    try {
      instance = createInstrumentation(requireFromProject, entry)
    } catch (error) {
      warnOnce(`load:${entry.name}`, `OpenTelemetry instrumentation ${entry.name} could not be loaded (${describeError(error).split('\n')[0]}); continuing without it. test.execution export remains enabled.`)
      continue
    }
    const modules = patchedModules(instance)
    if (modules && modules.every((name) => typeof name === 'string' && isBuiltin(name))) {
      instances.push(instance)
    } else {
      warnOnce(`package:${entry.name}`, `OpenTelemetry instrumentation ${entry.name} is not supported in Jest yet and was skipped: ` +
        "only instrumentation of Node's built-in modules, such as http and fetch, is supported. Test results are unaffected.")
    }
  }
  instrumentationSets.set(key, instances)
  return instances
}

// One async context manager per worker, registered wherever no working one
// is: the sandbox (for the tests' API) and the environment's realm (for
// instrumentation). It must be the same instance in both so that a
// test's context is visible to instrumentation in either realm.
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

const ensureContextManager = (api) => {
  if (hasContextManager(api)) return
  if (!api.context.setGlobalContextManager(contextManager())) throw new Error('could not register an async context manager')
}

// As OpenTelemetry's SDK setup does, so that an in-process server handling a
// test's request continues the test's trace. A registered propagator is
// never replaced.
const ensurePropagator = (api, propagator) => {
  if (api.propagation.fields().length === 0) api.propagation.setGlobalPropagator(propagator)
}

const hasTracerProvider = (api) => {
  const provider = api.trace.getTracerProvider()
  return !(provider instanceof api.ProxyTracerProvider) || provider.getDelegate() !== new api.ProxyTracerProvider().getDelegate()
}

// Some instrumentation (undici) also checks its config on every operation.
// Node's built-in modules are patched here rather than on their next
// require: require-in-the-middle remembers a module it has already returned
// unpatched, and the sandbox may never require it again. Recording the module
// on the instrumentation's own definition, as require-in-the-middle would,
// lets disable() unpatch it and a later enable() patch it again, each exactly
// once. Those are InstrumentationBase's `_modules`, the definitions its
// enable() and disable() walk; getModuleDefinitions() builds new ones on
// every call, which disable() never sees. Without them, built-ins are patched
// only through the instrumentation's own require hook.
const ownDefinitions = (instance) => (Array.isArray(instance._modules) ? instance._modules : [])

const enable = (instance, provider) => {
  instance.setTracerProvider(provider)
  instance.setConfig({ ...instance.getConfig(), enabled: true })
  instance.enable()
  for (const definition of ownDefinitions(instance)) {
    if (!isBuiltin(definition.name) || typeof definition.patch !== 'function') continue
    const exports = require(definition.name)
    if (definition.moduleExports === exports) continue
    definition.moduleExports = exports
    definition.patch(exports)
  }
}

// Connects one test file's sandbox to OpenTelemetry. Nothing is decided until
// its first test or beforeEach hook runs, after setup files, the file's own
// top-level code and beforeAll hooks, so a provider the suite registers wins:
// the collector then never replaces or reconfigures it and installs no
// instrumentation.
class JestOtelContext {
  constructor({ session, settings, testPath, rootDir }) {
    this.session = session
    this.entries = settings.instrumentations || []
    this.testPath = testPath
    this.rootDir = rootDir
    this.mode = null
    this.runtime = null
    this.bridgeApi = null
    this.traces = new Set()
    this.cleanups = []
    // Conditions every worker would hit are reported once per run.
    this.warnOnce = (key, message) => warnOncePerRun(settings.exportRecords, key, message)
  }

  // Called by the collector's Jest runtime when it is created for this file.
  attachRuntime(runtime) {
    this.runtime = runtime
    try {
      this.instrumentations = this.entries.length ? instrumentationSet(this.entries, this.rootDir, this.warnOnce) : null
    } catch (error) {
      warn(`OpenTelemetry instrumentation could not be loaded (${describeError(error)}); test.execution export remains enabled.`)
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

  _useCollectorProvider(api) {
    const trace = this.runtime.requireModule(this.testPath, '@opentelemetry/sdk-trace-base')
    ensureContextManager(this.session.api)
    ensureContextManager(api)
    ensurePropagator(this.session.api, this.session.childPropagator())
    ensurePropagator(api, this.session.childPropagator())
    this.forwarder = this.session.childExport().createForwarder()
    this.provider = new trace.BasicTracerProvider({ ...this.session.childProviderConfig, spanProcessors: [this.forwarder] })
    if (!api.trace.setGlobalTracerProvider(this.provider)) throw new Error('could not register the child tracer provider')
    for (const instance of this.instrumentations || []) {
      this.cleanups.push(() => instance.disable())
      enable(instance, this.provider)
    }
    this.mode = 'managed'
    for (const traceId of this.traces) this.forwarder.addTrace(traceId)
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
  // API and SDK into the sandbox.
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

  // Before Jest destroys the sandbox: no more forwarding, and built-in
  // modules are unpatched for the next file.
  async teardown() {
    const mode = this.mode
    this.mode = 'done'
    if (this.forwarder) await this.forwarder.shutdown()
    this._cleanUp()
    if (mode === 'managed' && this.provider) await this.provider.shutdown()
  }
}

module.exports = JestOtelContext
