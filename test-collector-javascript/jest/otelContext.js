const path = require('path')
const { createRequire } = require('module')
const { TEST_CONTEXT } = require('../util/otel/children')
const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')

const disabledMessage = (error) => `OpenTelemetry child span export disabled: ${describeError(error).split('\n')[0]}; test.execution export remains enabled`

// OpenTelemetry's instrumentation of Node's http and https modules and of
// fetch, from the 2.x SDK line the collector exports with: version 0.2xx, which
// instrumentation-undici numbers 0.11 to 0.33.
const INSTRUMENTATIONS = [
  { name: 'http', className: 'HttpInstrumentation', minors: [200, 300] },
  { name: 'undici', className: 'UndiciInstrumentation', minors: [11, 34] },
]

// The switches and short names of @opentelemetry/auto-instrumentations-node:
// a non-empty OTEL_NODE_ENABLED_INSTRUMENTATIONS keeps only the names it
// lists, then OTEL_NODE_DISABLED_INSTRUMENTATIONS removes the names it lists.
const listedIn = (variable, name) => (process.env[variable] || '').split(',').some((entry) => entry.trim() === name)
const enabledByEnv = (name) => (!process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS || listedIn('OTEL_NODE_ENABLED_INSTRUMENTATIONS', name)) &&
  !listedIn('OTEL_NODE_DISABLED_INSTRUMENTATIONS', name)

// Instrumentation is loaded once per worker, in the environment's realm (that
// of the built-in modules it patches), from the project, or else from the
// dependencies of @opentelemetry/auto-instrumentations-node, which pnpm does
// not hoist. Anything else is skipped with one warning per run.
const instrumentationSets = new Map()
const loadInstrumentations = (rootDir, warnOnce) => {
  if (instrumentationSets.has(rootDir)) return instrumentationSets.get(rootDir)
  const fromProject = createRequire(path.join(rootDir, 'package.json'))
  const loaders = [fromProject]
  try {
    loaders.push(createRequire(fromProject.resolve('@opentelemetry/auto-instrumentations-node')))
  } catch {
    // not installed
  }
  const instances = []
  const skipped = []
  for (const { name, className, minors } of INSTRUMENTATIONS.filter((entry) => enabledByEnv(entry.name))) {
    const packageName = `@opentelemetry/instrumentation-${name}`
    const rejected = []
    for (const load of loaders) {
      let instance
      try {
        instance = new (load(packageName)[className])({ enabled: false })
      } catch (error) {
        if (!(error.code === 'MODULE_NOT_FOUND' && error.message.startsWith(`Cannot find module '${packageName}'`))) {
          rejected.push(`${packageName} (${describeError(error).split('\n')[0]})`)
        }
        continue
      }
      const [major, minor] = String(instance.instrumentationVersion).split('.').map(Number)
      if (major === 0 && minor >= minors[0] && minor < minors[1]) {
        instances.push(instance)
        rejected.length = 0
        break
      }
      rejected.push(`${packageName} ${instance.instrumentationVersion}`)
    }
    skipped.push(...rejected)
  }
  if (skipped.length) {
    warnOnce('instrumentation', `OpenTelemetry instrumentation skipped: ${skipped.join(', ')}. The collector records child spans with ` +
      'instrumentation from the OpenTelemetry JavaScript 2.x SDK line: @opentelemetry/instrumentation-http 0.200 to 0.2xx and ' +
      '@opentelemetry/instrumentation-undici 0.11 to 0.33. test.execution export remains enabled.')
  }
  instrumentationSets.set(rootDir, instances)
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
// every call, which disable() never sees.
const enable = (instance, provider) => {
  instance.setTracerProvider(provider)
  instance.setConfig({ ...instance.getConfig(), enabled: true })
  instance.enable()
  for (const definition of Array.isArray(instance._modules) ? instance._modules : []) {
    if (typeof definition.patch !== 'function') continue
    const exports = require(definition.name)
    if (definition.moduleExports === exports) continue
    definition.moduleExports = exports
    definition.patch(exports)
  }
}

// Connects one test file's sandbox to OpenTelemetry when the collector's Jest
// runtime is set. Nothing is decided until its first test or beforeEach hook
// runs, after setup files, the file's own top-level code and beforeAll hooks.
// A provider the suite registers by then is never replaced or reconfigured,
// and the file exports test spans only.
class JestOtelContext {
  constructor({ session, settings, testPath, rootDir }) {
    this.session = session
    this.testPath = testPath
    this.rootDir = rootDir
    this.mode = null
    this.runtime = null
    this.traces = new Set()
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
      const api = this.runtime.requireModule(this.testPath, '@opentelemetry/api')
      // Also a provider registered outside the sandbox, for example with
      // NODE_OPTIONS=--require: instrumentation there sees every file's tests.
      if (hasTracerProvider(api) || hasTracerProvider(this.session.api)) {
        this.warnOnce('suite-provider', 'OpenTelemetry child spans are not exported because the suite registers its own tracer provider; ' +
          'to get them, let the collector provide the provider under Jest. test.execution export remains enabled.')
        return
      }
      this._useCollectorProvider(api)
    } catch (error) {
      this._cleanUp()
      this.mode = 'off'
      this.warnOnce(`setup:${describeError(error)}`, disabledMessage(error))
    }
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
    this.api = api
    this.mode = 'managed'
    for (const traceId of this.traces) this.forwarder.addTrace(traceId)
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
    if (this.mode !== 'managed' || !started?.handle) return fn.apply(receiver, args)
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

  // Before Jest destroys the sandbox: no more forwarding (children still open
  // are counted as dropped), and built-in modules are unpatched for the next
  // file.
  async teardown() {
    const managed = this.mode === 'managed'
    this.mode = 'done'
    if (this.forwarder) await this.forwarder.shutdown()
    this._cleanUp()
    if (managed) await this.provider.shutdown()
  }
}

module.exports = JestOtelContext
