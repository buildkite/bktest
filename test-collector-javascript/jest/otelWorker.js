const { isBuiltin, moduleDefinitions } = require('../util/otel/instrumentations')

// Where every copy of @opentelemetry/api 1.x keeps its globals.
const API_REGISTRY = Symbol.for('opentelemetry.js.api.1')

const PROBE = Symbol.for('buildkite.test.execution.probe')
const hasContextManager = (api) => {
  const probe = api.ROOT_CONTEXT.setValue(PROBE, true)
  return api.context.with(probe, () => api.context.active().getValue(PROBE)) === true
}

// As OpenTelemetry's SDK setup does, so that an in-process server handling a
// test's request continues the test's trace. A registered propagator is
// never replaced.
const ensurePropagator = (api, propagator) => {
  if (api.propagation.fields().length === 0) api.propagation.setGlobalPropagator(propagator)
}

// Records only while a file the collector manages is running. Otherwise it
// behaves as OpenTelemetry's API does when no SDK is registered: spans are
// non-recording and only carry on the current context. Instrumentation
// patched for the worker therefore records nothing in a file that opted out
// of the collector's environment or owns its provider, and only propagates
// a trace that is already current.
const gatedTracer = (isOpen, tracer, noop) => ({
  startSpan: (...args) => (isOpen() ? tracer : noop).startSpan(...args),
  startActiveSpan: (...args) => (isOpen() ? tracer : noop).startActiveSpan(...args),
})

// Built-in modules are shared by every test file the worker runs, so their
// instrumentation is enabled once per worker, as a preload would, and never
// unpatched. require-in-the-middle caches a module the first time it sees it
// and never hands that module to instrumentation registered later, so each
// built-in is required first, while nothing is registered for it, and then
// patched here: exactly once, whether or not anything required it before.
const enableShared = (shared, tracerProvider) => {
  const builtins = new Map()
  for (const { instance } of shared) {
    for (const definition of moduleDefinitions(instance)) {
      if (isBuiltin(definition.name) && typeof definition.patch === 'function') builtins.set(definition, instance)
    }
  }
  for (const definition of builtins.keys()) require(definition.name)
  for (const { instance } of shared) {
    instance.setTracerProvider(tracerProvider)
    instance.setConfig({ ...instance.getConfig(), enabled: true })
    instance.enable()
  }
  for (const definition of builtins.keys()) definition.patch(require(definition.name))
}

// One child tracer provider per worker process, in the environment's realm,
// for every test file the worker runs: the provider the collector registers
// in each managed file's sandbox, and the one the shared instrumentation
// records with. Each span goes to the forwarder of the file that was running
// when it started, which keeps it only if it belongs to one of that file's
// test traces.
const createWorker = (session, shared) => {
  const { api } = session
  let current = null
  const owners = new WeakMap()
  const dispatcher = {
    onStart(span, context) {
      if (!current) return
      owners.set(span, current.forwarder)
      current.forwarder.onStart(span, context)
    },
    onEnd(span) {
      const forwarder = owners.get(span)
      if (!forwarder) return
      owners.delete(span)
      forwarder.onEnd(span)
    },
    forceFlush: async () => {},
    shutdown: async () => {},
  }
  const provider = session.createChildProvider([dispatcher])
  const tracers = new Map()
  const isOpen = () => current !== null
  const tracerProvider = {
    getTracer(name, version, options) {
      const key = JSON.stringify([name, version, options?.schemaUrl])
      if (!tracers.has(key)) {
        const noop = new api.ProxyTracerProvider().getTracer(name, version, options)
        tracers.set(key, gatedTracer(isOpen, provider.getTracer(name, version, options), noop))
      }
      return tracers.get(key)
    },
  }

  if (!hasContextManager(api)) {
    const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
    if (!api.context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable())) {
      throw new Error('could not register an async context manager')
    }
  }
  // The same instance goes into every sandbox, so a test's context is
  // visible to instrumentation and tracers in either realm.
  const contextManager = globalThis[API_REGISTRY].context
  ensurePropagator(api, session.childPropagator())
  enableShared(shared, tracerProvider)

  return {
    tracerProvider,
    contextManager,
    // Connects a test file's sandbox and starts recording for it.
    open(file, sandboxApi) {
      if (!hasContextManager(sandboxApi) && !sandboxApi.context.setGlobalContextManager(contextManager)) {
        throw new Error('could not register an async context manager')
      }
      ensurePropagator(sandboxApi, session.childPropagator())
      if (!sandboxApi.trace.setGlobalTracerProvider(tracerProvider)) throw new Error('could not register the child tracer provider')
      current = file
    },
    close(file) {
      if (current === file) current = null
    },
  }
}

// Set up by the first file the collector manages in this process, with that
// file's shared instrumentation. A failure is not retried: setup may have
// patched some built-ins already.
let worker = null
const workerFor = (session, shared) => {
  if (!worker) {
    try {
      worker = { value: createWorker(session, shared) }
    } catch (error) {
      worker = { error }
    }
  }
  if (worker.error) throw worker.error
  return worker.value
}

module.exports = { workerFor, hasContextManager }
