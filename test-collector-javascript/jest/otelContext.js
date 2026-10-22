const { TEST_CONTEXT } = require('../util/otel/children')
const { warn, describeError } = require('../util/otel/warn')

class JestOtelContext {
  constructor(session) {
    this.session = session
    this.options = {}
    this.patches = []
    this.cleanups = []
  }

  connect(sdk) { this.sdk = sdk }
  configure(options) { this.options = options }
  disable(error) {
    this.failed = true
    warn(`OpenTelemetry child span export disabled: ${describeError(error)}; test.execution export remains enabled`)
  }

  createSpanProcessor() {
    try {
      if (!this.forwarder) {
        this.forwarder = this.session.childExport().createForwarder(this.sdk.api)
      }
      return this.forwarder
    } catch (error) {
      this.disable(error)
      return { onStart() {}, onEnd() {}, forceFlush: async () => {}, shutdown: async () => {} }
    }
  }

  instrumentModule(instrumentation, name, exports, version) {
    this.patches.push({ instrumentation, name, exports, version })
  }

  setup() {
    if (this.started || this.failed || !this.sdk) return
    this.started = true
    try {
      const { api, trace, ContextManager } = this.sdk
      const provider = api.trace.getTracerProvider()
      const unconfigured = provider instanceof api.ProxyTracerProvider &&
        provider.getDelegate() === new api.ProxyTracerProvider().getDelegate()
      if (unconfigured) {
        this.provider = this.session.childExport().createProvider(trace, this.createSpanProcessor())
        if (!api.trace.setGlobalTracerProvider(this.provider)) throw new Error('could not register the child provider')
      } else if (!this.forwarder) {
        warn('OpenTelemetry child span export disabled: suite-owned SDK 2.x provider requires createBuildkiteSpanProcessor() in spanProcessors; test.execution export remains enabled')
      }

      const probe = api.ROOT_CONTEXT.setValue(TEST_CONTEXT, 'probe')
      if (!api.context.with(probe, () => api.context.active().getValue(TEST_CONTEXT))) {
        this.contextManager = new ContextManager().enable()
        if (!api.context.setGlobalContextManager(this.contextManager)) throw new Error('could not register an async context manager')
      }
      if (unconfigured) {
        for (const instrumentation of this.options.instrumentations || []) {
          instrumentation.setTracerProvider(this.provider)
          instrumentation.setConfig({ ...instrumentation.getConfig(), enabled: true })
          instrumentation.enable()
          this.cleanups.push(() => instrumentation.disable())
        }
      }
      // Explicit requests from the suite, including file-level definitions
      // (e.g. graphql/execution/execute.js), not auto-discovered modules.
      for (const { instrumentation, name, exports, version } of this.patches) {
        const definitions = instrumentation.getModuleDefinitions()
        const definition = definitions.flatMap((d) => [d, ...(d.files || [])]).find((d) => d.name === name)
        if (!definition?.patch) throw new Error(`no module patch definition for ${name}`)
        const { satisfies } = require('semver')
        if (!definition.supportedVersions.some((range) => satisfies(version, range))) {
          throw new Error(`unsupported ${name} version ${version}`)
        }
        definition.patch(exports, version)
        this.cleanups.push(() => definition.unpatch?.(exports, version))
      }
    } catch (error) {
      this.disable(error)
    }
  }

  run(started, fn, receiver, args) {
    if (!started?.handle || !this.sdk || this.failed) return fn.apply(receiver, args)
    const { api } = this.sdk
    const span = started.handle.span
    const context = api.trace.setSpan(api.context.active(), span).setValue(TEST_CONTEXT, span.spanContext().traceId)
    const result = api.context.with(context, fn, receiver, ...args)
    if (fn.constructor?.name === 'GeneratorFunction') {
      for (const method of ['next', 'throw', 'return']) {
        result[method] = api.context.bind(context, result[method].bind(result))
      }
    }
    return result
  }

  async teardown() {
    if (this.forwarder) await this.forwarder.shutdown()
    for (const cleanup of this.cleanups.reverse()) {
      try { cleanup() } catch (error) { warn(`Could not unpatch OpenTelemetry instrumentation: ${describeError(error)}`) }
    }
    if (this.provider) await this.provider.shutdown()
    if (this.contextManager) this.contextManager.disable()
  }
}

module.exports = JestOtelContext
