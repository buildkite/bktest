// A worker's built-ins stay patched, but only record while a managed file
// owns the gate. Spans retain the forwarder of the file where they began.
const { isBuiltin } = require('module')

let worker
const enabled = new Set()
const workerFor = (session, instances) => {
  if (worker?.error) throw worker.error
  try {
    if (!worker) {
      let current = null
      const owners = new WeakMap()
      const dispatcher = {
        onStart(span, context) {
          if (!current) return
          owners.set(span, current.forwarder)
          current.forwarder.onStart(span, context)
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
      const tracerProvider = {
        getTracer(...args) {
          const tracer = provider.getTracer(...args)
          const noop = new session.api.ProxyTracerProvider().getTracer(...args)
          const active = () => current && (!enabled.has(args[0]) || worker.selected(args[0]))
          return {
            startSpan: (...args) => (active() ? tracer : noop).startSpan(...args),
            startActiveSpan: (...args) => (active() ? tracer : noop).startActiveSpan(...args),
          }
        },
      }
      worker = {
        tracerProvider,
        selected(name) { return Boolean(current?.instrumentations?.some((entry) => entry.instance.instrumentationName === name)) },
        open(file) { current = file },
        close(file) { if (current === file) current = null },
      }
    }
    // Preload before enabling require-in-the-middle, then patch definitions
    // it did not see. Use the instance's actual definitions, not fresh ones.
    // Later projects can discover new instrumentations, but another copy of
    // an already-enabled instrumentation must not wrap built-ins twice.
    for (const instance of instances) {
      if (enabled.has(instance.instrumentationName)) continue
      for (const definition of instance._modules || []) if (isBuiltin(definition.name)) require(definition.name)
      instance.setTracerProvider(worker.tracerProvider)
      const config = { ...instance.getConfig(), enabled: true }
      if (instance.instrumentationName === '@opentelemetry/instrumentation-http') {
        // Bypass HTTP before upstream argument normalization, not just span
        // creation: inactive callers must retain Node's validation behavior.
        const wrap = instance._wrap.bind(instance)
        instance._wrap = (exports, name, wrapper) => wrap(exports, name, (original, ...args) => new Proxy(wrapper(original, ...args), {
          apply: (patched, receiver, args) => Reflect.apply(worker.selected(instance.instrumentationName) ? patched : original, receiver, args),
        }))
      }
      if (instance.instrumentationName === '@opentelemetry/instrumentation-undici') {
        // Ignore new requests without unsubscribing callbacks that finish
        // requests already started in an earlier file.
        config.ignoreRequestHook = () => !worker.selected(instance.instrumentationName)
      }
      instance.setConfig(config)
      instance.enable()
      for (const definition of instance._modules || []) {
        if (!isBuiltin(definition.name) || typeof definition.patch !== 'function') continue
        const exports = require(definition.name)
        if (definition.moduleExports === exports) continue
        definition.moduleExports = exports
        definition.patch(exports)
      }
      enabled.add(instance.instrumentationName)
    }
  } catch (error) {
    worker = { error }
    throw error
  }
  return worker
}

module.exports = { workerFor }
