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
          return {
            startSpan: (...args) => (current ? tracer : noop).startSpan(...args),
            startActiveSpan: (...args) => (current ? tracer : noop).startActiveSpan(...args),
          }
        },
      }
      worker = {
        tracerProvider,
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
      instance.setConfig({ ...instance.getConfig(), enabled: true })
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
