const { isBuiltin, syncBuiltinESMExports } = require('module')
const activated = new WeakSet()
const httpWrappers = new WeakMap()

const activateBuiltins = (instances, tracerProvider, recording) => {
  // There is no public hook to re-patch already-loaded builtins or bypass
  // HTTP before argument normalization. Use live _modules/patch and _wrap;
  // getModuleDefinitions() returns fresh copies, not the require hook's state.
  for (const instance of instances) {
    if (!activated.has(instance)) {
      // Preload before enabling require-in-the-middle.
      for (const definition of instance._modules || []) if (isBuiltin(definition.name)) require(definition.name)
      instance.setTracerProvider(tracerProvider)
      const config = { ...instance.getConfig(), enabled: true }
      if (instance.instrumentationName === '@opentelemetry/instrumentation-http') {
        // Inactive callers must retain Node's validation behavior.
        const wrap = instance._wrap.bind(instance)
        let requesting = false
        instance._wrap = (exports, name, wrapper) => {
          // Only gate outgoing calls. A server started outside a test still
          // receives requests carrying that test's traceparent.
          if (name !== 'request' && name !== 'get') return wrap(exports, name, wrapper)
          const patched = wrap(exports, name, (original, ...args) => new Proxy(wrapper(original, ...args), {
            apply(patched, receiver, args) {
              if (!recording(instance.instrumentationName) || (name === 'request' && requesting)) return Reflect.apply(original, receiver, args)
              // An interceptor may retain an intact older wrapper. Only
              // request owns the guard: patched get calls patched request.
              if (name !== 'request') return Reflect.apply(patched, receiver, args)
              requesting = true
              try { return Reflect.apply(patched, receiver, args) } finally { requesting = false }
            },
          }))
          if (!httpWrappers.has(exports)) httpWrappers.set(exports, {})
          httpWrappers.get(exports)[name] = patched
          return patched
        }
      }
      if (instance.instrumentationName === '@opentelemetry/instrumentation-undici') {
        // Ignore new requests without unsubscribing callbacks that finish
        // requests already started in an earlier file.
        config.ignoreRequestHook = () => !recording(instance.instrumentationName)
      }
      instance.setConfig(config)
      instance.enable()
      activated.add(instance)
    }
    for (const definition of instance._modules || []) {
      if (!isBuiltin(definition.name) || typeof definition.patch !== 'function') continue
      const exports = require(definition.name)
      const wrappers = httpWrappers.get(exports)
      if (definition.moduleExports === exports) {
        if (!wrappers || ['request', 'get'].every((name) => exports[name] === wrappers[name])) continue
        // Reset HTTP's patch guard without unwrapping interceptor replacements.
        definition.unpatch()
      }
      definition.moduleExports = exports
      definition.patch(exports)
    }
  }
  syncBuiltinESMExports()
}

module.exports = { activateBuiltins }
