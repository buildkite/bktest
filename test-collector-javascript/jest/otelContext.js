const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')
const { worker, workerFor } = require('./otelWorker')
const JestOtelPackages = require('./otelPackages')
const { loadInstrumentations } = require('../util/otel/instrumentations')
const { createContextManager } = require('../util/otel/sdk')

const disabledMessage = (error) => `OpenTelemetry child span export disabled: ${describeError(error).split('\n')[0]}; test.execution export remains enabled`
const SUITE_PROVIDER = 'OpenTelemetry child spans are not exported because the suite registers its own tracer provider; ' +
  'to get them, let the collector provide the provider under Jest. test.execution export remains enabled.'

// Tests and instrumentation in different realms must share async context.
const contextManager = () => {
  return worker.contextManager ||= createContextManager()
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

// Registration would block the suite's own manager. Fill only the API's noop
// fallback, which a suite manager can replace without involving the collector.
const fillContextManager = (api) => {
  if (hasContextManager(api)) return
  const fallback = api.context._getContextManager()
  if (fallback.constructor.name === 'NoopContextManager') {
    const manager = contextManager()
    for (const method of ['active', 'with', 'bind']) fallback[method] = manager[method].bind(manager)
  }
  if (!hasContextManager(api)) throw new Error('could not set up an async context manager')
}

// Delegation preserves early tracers without blocking suite registration.
// null means a suite-owned provider must be left alone.
const emptyProxies = (apis, ours) => {
  const empty = []
  for (const api of apis) {
    const proxy = api.trace.getTracerProvider()
    const delegate = proxy.getDelegate()
    if (delegate === new api.ProxyTracerProvider().getDelegate()) empty.push(proxy)
    else if (delegate !== ours?.get(proxy)) return null
  }
  return empty
}

class JestOtelContext {
  constructor({ session, settings, testPath, rootDir }) {
    this.session = session
    this.testPath = testPath
    this.rootDir = rootDir
    this.mode = null
    this.runtime = null
    this.traces = new Set()
    this.apis = new Set()
    this.providers = new Map()
    this.cleanups = []
    this.warnOnce = (key, message) => warnOncePerRun(settings.spool, key, message)
  }

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
    return this.worker ||= workerFor(this.session, (this.instrumentations || []).map((entry) => entry.instance))
  }

  load(file, load) {
    // A suite-owned provider must not activate collector package patches.
    if (!emptyProxies([...this.apis, this.session.api], this.providers)) return load()
    return this.packages.load(file, load)
  }

  loadedApi(api) {
    if (this.apis.has(api) || api?.trace?.getTracerProvider?._isMockFunction) return
    try {
      if (typeof api.trace.getTracerProvider().setDelegate !== 'function') return
    } catch {
      return // a mocked API
    }
    this.apis.add(api)
    if (this.mode === 'managed') this._fill()
  }

  setup() {
    if (this.mode !== null) return
    this.mode = 'off'
    try {
      this.api = this.runtime.requireModule(this.testPath, '@opentelemetry/api')
      this.loadedApi(this.api)
      if (!this.apis.has(this.api)) this.api = this.session.api // a mapped or manual mock
      // NODE_OPTIONS can register a provider outside the sandbox too.
      if (!emptyProxies([...this.apis, this.session.api])) {
        this.warnOnce('suite-provider', SUITE_PROVIDER)
        return
      }
      ensureContextManager(this.session.api)
      // Let in-process servers continue the test trace without changing the suite's propagator.
      if (this.session.api.propagation.fields().length === 0) this.session.api.propagation.setGlobalPropagator(this.session.childPropagator())
      this.forwarder = this.session.childExport().createForwarder()
      this.provider = this._worker().tracerProvider
      this.worker.refresh()
      this.mode = 'managed'
      for (const traceId of this.traces) this.forwarder.addTrace(traceId)
      this.api.diag?.debug('buildkite-test-collector: OpenTelemetry instrumentations selected for this file:', (this.instrumentations || []).map((entry) => entry.name).sort().join(', ') || '(none)')
    } catch (error) {
      this._cleanUp()
      this.mode = 'off'
      this.warnOnce(`setup:${describeError(error)}`, disabledMessage(error))
    }
  }

  _fill() {
    try {
      const empty = emptyProxies(this.apis, this.providers)
      if (!empty) {
        this.worker.close(this)
        this.warnOnce('suite-provider', SUITE_PROVIDER)
        return false
      }
      for (const api of this.apis) fillContextManager(api)
      for (const proxy of empty) {
        const provider = { getTracer: (...args) => {
          const tracer = this.provider.getTracer(...args)
          // Cached API tracers must follow a later suite-owned delegate.
          const current = () => proxy.getDelegate() === provider ? tracer : proxy.getDelegate().getTracer(...args)
          return {
            startSpan: (...args) => current().startSpan(...args),
            startActiveSpan: (...args) => current().startActiveSpan(...args),
          }
        } }
        this.providers.set(proxy, provider)
        proxy.setDelegate(provider)
      }
      this.worker.open(this)
      return true
    } catch (error) {
      this.warnOnce(`setup:${describeError(error)}`, disabledMessage(error))
      return false
    }
  }

  began(started) {
    const traceId = started?.handle?.span?.spanContext?.().traceId
    if (!traceId) return
    this.traces.add(traceId)
    if (this.mode === 'managed') this.forwarder.addTrace(traceId)
  }

  // Jest 30.5 refuses module loads between tests, so defer setup to test code.
  run(started, fn, receiver, args) {
    this.setup()
    if (this.mode !== 'managed' || !started?.handle || !this._fill()) return fn.apply(receiver, args)
    const { span } = started.handle
    const contexts = [...new Set([this.api, this.session.api])].map((api) => [api, api.trace.setSpan(api.context.active(), api.trace.wrapSpanContext(span.spanContext()))])
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

  // Close before destroying the sandbox; already-started children keep their owner.
  async teardown() {
    this.mode = 'done'
    this.worker?.close(this)
    if (this.forwarder) await this.forwarder.shutdown()
    this._cleanUp()
  }
}

module.exports = JestOtelContext
