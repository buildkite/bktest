// A worker's built-ins stay patched, but only record while a managed file
// owns the gate. Spans retain the forwarder of the file where they began.
const { isBuiltin } = require('module')
const Debug = require('../util/debug')

let worker
const enabled = new Map()
const httpWrappers = new WeakMap()
let requesting = false
const workerFor = (session, instances) => {
  if (worker?.error) throw worker.error
  try {
    if (!worker) {
      // Log the union once at exit, including selections from later projects.
      process.once('exit', () => Debug.log(`OpenTelemetry instrumentations selected in this worker: ${[...enabled.keys()].sort().join(', ') || '(none)'}`))
      let current = null
      const owners = new WeakMap()
      const dispatcher = {
        onStart(span, context) {
          if (session.childExport().rejectLate(span)) return
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
          // Outside a managed file, only closed test traces reach the
          // dispatcher, so late children count without tracing suite work.
          const active = (parent) => current
            ? !enabled.has(args[0]) || worker.selected(args[0])
            : !enabled.has(args[0]) && session.childExport().isClosedTrace(
              session.api.trace.getSpanContext(parent || session.api.context.active())?.traceId)
          return {
            startSpan: (...args) => (active(args[2]) ? tracer : noop).startSpan(...args),
            startActiveSpan: (...args) => (active(args.length === 4 ? args[2] : undefined) ? tracer : noop).startActiveSpan(...args),
          }
        },
      }
      worker = {
        tracerProvider,
        selected(name) { return Boolean(current?.instrumentations?.some((entry) => entry.instance.instrumentationName === name)) },
        refreshHttp() {
          const instance = enabled.get('@opentelemetry/instrumentation-http')
          for (const definition of instance?._modules || []) {
            const exports = require(definition.name)
            const wrappers = httpWrappers.get(exports)
            if (!wrappers || ['request', 'get'].every((name) => exports[name] === wrappers[name])) continue
            // Reset upstream's patch guard without unwrapping interceptor
            // replacements. Its patch replaces any remaining old wrappers.
            definition.unpatch()
            definition.patch(exports)
          }
        },
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
        instance._wrap = (exports, name, wrapper) => {
          const patched = wrap(exports, name, (original, ...args) => new Proxy(wrapper(original, ...args), {
            apply(patched, receiver, args) {
              if (!worker.selected(instance.instrumentationName) || (name === 'request' && requesting)) return Reflect.apply(original, receiver, args)
              // An interceptor may retain an intact older wrapper. Only
              // request owns the guard: patched get calls patched request.
              if (name !== 'request') return Reflect.apply(patched, receiver, args)
              requesting = true
              try { return Reflect.apply(patched, receiver, args) } finally { requesting = false }
            },
          }))
          if (name === 'request' || name === 'get') {
            if (!httpWrappers.has(exports)) httpWrappers.set(exports, {})
            httpWrappers.get(exports)[name] = patched
          }
          return patched
        }
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
      enabled.set(instance.instrumentationName, instance)
    }
  } catch (error) {
    worker = { error }
    throw error
  }
  return worker
}

module.exports = { workerFor }
