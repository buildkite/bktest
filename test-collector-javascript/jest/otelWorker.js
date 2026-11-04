const { isBuiltin } = require('module')
const Debug = require('../util/debug')

const worker = {
  enabled: new Map(), httpWrappers: new WeakMap(), sets: new Map(),
  current: null, requesting: false,
  open(file) { this.current = file },
  close(file) { if (this.current === file) this.current = null },
  refresh() { patchBuiltins(this.enabled.values()) },
}

const createProvider = (session) => {
  const owners = new WeakMap()
  // Instrumentation is project-specific even when Jest shares a worker.
  // Outside a managed file, only manual children of closed traces reach
  // the dispatcher, so they count as late without tracing suite work.
  worker.recording = (name, parent) => {
    if (worker.current) return !worker.enabled.has(name) || worker.current.instrumentations?.some((entry) => entry.instance.instrumentationName === name)
    return !worker.enabled.has(name) && Boolean(parent) && session.childExport().isClosedTrace(session.api.trace.getSpanContext(parent)?.traceId)
  }
  const dispatcher = {
    onStart(span) {
      if (session.childExport().rejectLate(span) || !worker.current) return
      owners.set(span, worker.current.forwarder)
      worker.current.forwarder.onStart(span)
    },
    onEnd(span) {
      owners.get(span)?.onEnd(span)
      owners.delete(span)
    },
    forceFlush: async () => {},
    shutdown: async () => {},
  }
  const { BasicTracerProvider } = require('@opentelemetry/sdk-trace-base')
  const provider = new BasicTracerProvider({ ...session.childProviderConfig, spanProcessors: [dispatcher] })
  worker.tracerProvider = {
    getTracer(...args) {
      const tracer = provider.getTracer(...args)
      const noop = new session.api.ProxyTracerProvider().getTracer(...args)
      const active = (parent) => worker.recording(args[0], parent || session.api.context.active())
      return {
        startSpan: (...args) => (active(args[2]) ? tracer : noop).startSpan(...args),
        startActiveSpan: (...args) => (active(args.length === 4 ? args[2] : undefined) ? tracer : noop).startActiveSpan(...args),
      }
    },
  }
  process.once('exit', () => Debug.log(`OpenTelemetry instrumentations selected in this worker: ${[...worker.enabled.keys()].sort().join(', ') || '(none)'}`))
}

const patchBuiltins = (instances) => {
  // Use live definitions, not getModuleDefinitions()'s fresh copies: the
  // require hook and this path must agree on which exports are patched.
  for (let instance of instances) {
    instance = worker.enabled.get(instance.instrumentationName) || instance
    if (!worker.enabled.has(instance.instrumentationName)) {
      // Preload before enabling require-in-the-middle.
      for (const definition of instance._modules || []) if (isBuiltin(definition.name)) require(definition.name)
      instance.setTracerProvider(worker.tracerProvider)
      const config = { ...instance.getConfig(), enabled: true }
      if (instance.instrumentationName === '@opentelemetry/instrumentation-http') {
        // Bypass HTTP before upstream argument normalization, not just span
        // creation: inactive callers must retain Node's validation behavior.
        const wrap = instance._wrap.bind(instance)
        instance._wrap = (exports, name, wrapper) => {
          const patched = wrap(exports, name, (original, ...args) => new Proxy(wrapper(original, ...args), {
            apply(patched, receiver, args) {
              if (!worker.recording(instance.instrumentationName) || (name === 'request' && worker.requesting)) return Reflect.apply(original, receiver, args)
              // An interceptor may retain an intact older wrapper. Only
              // request owns the guard: patched get calls patched request.
              if (name !== 'request') return Reflect.apply(patched, receiver, args)
              worker.requesting = true
              try { return Reflect.apply(patched, receiver, args) } finally { worker.requesting = false }
            },
          }))
          if (name === 'request' || name === 'get') {
            if (!worker.httpWrappers.has(exports)) worker.httpWrappers.set(exports, {})
            worker.httpWrappers.get(exports)[name] = patched
          }
          return patched
        }
      }
      if (instance.instrumentationName === '@opentelemetry/instrumentation-undici') {
        // Ignore new requests without unsubscribing callbacks that finish
        // requests already started in an earlier file.
        config.ignoreRequestHook = () => !worker.recording(instance.instrumentationName)
      }
      instance.setConfig(config)
      instance.enable()
    }
    for (const definition of instance._modules || []) {
      if (!isBuiltin(definition.name) || typeof definition.patch !== 'function') continue
      const exports = require(definition.name)
      const wrappers = worker.httpWrappers.get(exports)
      if (definition.moduleExports === exports) {
        if (!wrappers || ['request', 'get'].every((name) => exports[name] === wrappers[name])) continue
        // Reset HTTP's patch guard without unwrapping interceptor replacements.
        definition.unpatch()
      }
      definition.moduleExports = exports
      definition.patch(exports)
    }
    worker.enabled.set(instance.instrumentationName, instance)
  }
}

const workerFor = (session, instances) => {
  if (worker.error) throw worker.error
  try {
    if (!worker.tracerProvider) createProvider(session)
    patchBuiltins(instances)
  } catch (error) {
    worker.error = error
    throw error
  }
  return worker
}

module.exports = { worker, workerFor }
