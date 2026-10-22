// Loaded by setupFilesAfterEnv, in the SAME realm/module registry as the tests.
// Functions and SDK objects cross to the environment directly, never via JSON.
const bridge = globalThis.__buildkiteOtel
if (bridge) {
  try {
    bridge.connect({
      api: require('@opentelemetry/api'),
      trace: require('@opentelemetry/sdk-trace-base'),
      ContextManager: require('@opentelemetry/context-async-hooks').AsyncLocalStorageContextManager,
    })
  } catch (error) {
    bridge.disable(error)
  }
}

exports.configure = (options = {}) => {
  if (bridge) bridge.configure(options)
}

// SDK 2.x has no public addSpanProcessor. Suites install this in their own
// provider's constructor; we never change their sampler, resource or exporters.
exports.createBuildkiteSpanProcessor = () => bridge
  ? bridge.createSpanProcessor()
  : { onStart() {}, onEnd() {}, forceFlush: async () => {}, shutdown: async () => {} }

// Jest's userland require bypasses require-in-the-middle. Use the public
// (experimental) module-definition API to patch the actual Jest-loaded exports.
exports.instrumentModule = (instrumentation, name, exports, version) => {
  if (bridge) bridge.instrumentModule(instrumentation, name, exports, version)
  return exports
}
