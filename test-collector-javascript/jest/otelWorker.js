// A worker's built-ins stay patched, but only record while a managed file
// owns the gate. Spans retain the forwarder of the file where they began.
let worker
const workerFor = (session, instances) => {
  if (worker) {
    if (worker.error) throw worker.error
    return worker
  }
  try {
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
    // Preload before enabling require-in-the-middle, then patch definitions
    // it did not see. Use the instance's actual definitions, not fresh ones.
    for (const instance of instances) {
      for (const definition of instance._modules || []) require(definition.name)
    }
    for (const instance of instances) {
      instance.setTracerProvider(tracerProvider)
      instance.setConfig({ ...instance.getConfig(), enabled: true })
      instance.enable()
      for (const definition of instance._modules || []) {
        if (typeof definition.patch !== 'function') continue
        const exports = require(definition.name)
        if (definition.moduleExports === exports) continue
        definition.moduleExports = exports
        definition.patch(exports)
      }
    }
    worker = {
      tracerProvider,
      open(file) { current = file },
      close(file) { if (current === file) current = null },
    }
  } catch (error) {
    worker = { error }
    throw error
  }
  return worker
}

module.exports = { workerFor }
