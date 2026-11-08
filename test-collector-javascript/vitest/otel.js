const fs = require('fs')
const { isMainThread } = require('worker_threads')
const { isBuiltin, syncBuiltinESMExports } = require('module')
const { readSettings } = require('../util/otel/config')
const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')

const createWorker = (settings) => {
  const [rootDir, project] = JSON.parse(process.env.BUILDKITE_TEST_COLLECTOR_VITEST_PROJECT)
  const { loadSdk, createProvider, createResource, createContextManager, createPropagator } = require('../util/otel/sdk')
  const sdk = loadSdk({ exporter: false, context: true })
  const { api } = sdk
  const { createChildExport } = require('../util/otel/children')
  const { commitFile } = require('../util/otel/spool')
  const testIds = require('./otelIds')
  const recordRoot = (traceId) => {
    try { fs.appendFileSync(testIds.rootsFile(settings.spool, project), traceId ? `${traceId}\n` : '') }
    catch { /* The check is advisory; failed child commits warn on their own. */ }
  }
  const children = createChildExport(settings)
  const roots = new Map()
  const nodes = new Map()
  const files = new Map()
  let primed

  const flush = () => {
    const spans = children.take()
    try { commitFile(settings.spool, undefined, { child: spans }) }
    catch (error) { warn(`OpenTelemetry dropped ${spans.length} child span(s): ${describeError(error)}; test.execution results are unaffected.`) }
  }

  // Admit phases with test-owned children, errors or events. Empty phases
  // never consume the per-file child budget.
  const forward = (node) => {
    if (!node || node.root || node.sent !== undefined) return
    node.sent = node.forwarder.onStart(node.span)
    if (!node.sent) return
    forward(node.parent)
    let parent = node.parent
    while (parent && !parent.root && !parent.sent) parent = parent.parent
    node.span.parentSpanContext = parent?.span.spanContext()
    if (node.ended) node.forwarder.onEnd(node.span)
  }
  const processor = {
    onStart(span) {
      const context = span.spanContext()
      if (children.rejectLate(span)) return
      const root = roots.get(context.traceId)
      if (!root) return
      const parent = nodes.get(span.parentSpanContext?.spanId) || nodes.get(root.id)
      const node = { span, parent, forwarder: root.forwarder, root: context.spanId === root.id }
      if (!node.root && !(span.instrumentationScope.name === 'vitest' && span.name.startsWith('vitest.'))) {
        forward(node)
        if (!node.sent) return
      }
      nodes.set(context.spanId, node)
      root.nodes.push(context.spanId)
    },
    onEnd(span) {
      const context = span.spanContext()
      const node = nodes.get(context.spanId)
      if (!node) return
      if (!node.root && (span.status.code === api.SpanStatusCode.ERROR || span.events.length)) forward(node)
      node.ended = true
      if (node.sent) node.forwarder.onEnd(span)
      if (node.root) {
        children.closeTrace(context.traceId)
        // Retain only open spans, so late completions are still accounted for.
        for (const id of roots.get(context.traceId).nodes) if (nodes.get(id)?.ended) nodes.delete(id)
        roots.delete(context.traceId)
        // Reporter callbacks can run before Vitest calls SDK shutdown.
        flush()
      } else if (children.isClosedTrace(context.traceId)) {
        nodes.delete(context.spanId)
        flush()
      }
    },
    async forceFlush() {}, async shutdown() {},
  }
  const provider = createProvider(sdk, {
    resource: createResource(sdk, settings.runEnv),
    getIds: () => primed,
    spanProcessors: [processor],
  })
  const noop = new api.ProxyTracerProvider().getTracer('buildkite-test-collector')
  const wrappedProvider = { getTracer(...args) {
    const sdkTracer = provider.getTracer(...args)
    const tracer = Object.create(sdkTracer)
    // Gate suite work without overriding the SDK's environment-selected sampler.
    tracer.startSpan = (name, options, context = api.context.active()) => {
      const id = api.trace.getSpanContext(context)?.traceId
      return (primed || roots.has(id) || children.isClosedTrace(id) ? sdkTracer : noop).startSpan(name, options, context)
    }
    if (args[0] !== 'vitest') return tracer
    const wrapped = Object.create(tracer)
    wrapped.startSpan = (name, options, context) => {
      const id = options?.attributes?.['vitest.test.id']
      if (id === undefined) return tracer.startSpan(name, options, context)
      const file = options.attributes['code.file.path']
      if (!files.has(file)) files.set(file, children.createForwarder())
      const forwarder = files.get(file)
      primed = testIds(settings.seed, rootDir, project, file, id)
      recordRoot(primed.traceId)
      forwarder.addTrace(primed.traceId)
      roots.set(primed.traceId, { id: primed.spanId, forwarder, nodes: [] })
      try {
        const span = tracer.startSpan(name, { ...options, root: true }, api.ROOT_CONTEXT)
        if (!span.isRecording()) roots.delete(primed.traceId)
        return span
      }
      finally { primed = null }
    }
    return wrapped
  } }
  const manager = createContextManager(sdk)
  if (!api.trace.setGlobalTracerProvider(wrappedProvider)) {
    manager.disable()
    warnOncePerRun(settings.spool, 'vitest-provider', 'Vitest child spans are disabled because a tracer provider is already registered; let the collector own sdkPath. test.execution export remains enabled.')
    return { async shutdown() {} }
  }
  api.context.setGlobalContextManager(manager)
  // An empty record tells the reporter this project's workers can recognise tests.
  recordRoot()
  api.propagation.setGlobalPropagator(createPropagator(sdk))
  try {
    const { loadInstrumentations } = require('../util/otel/instrumentations')
    for (const { instance } of loadInstrumentations(rootDir,
      (key, message) => warnOncePerRun(settings.spool, key, message), ['http', 'undici'])) {
      instance.setTracerProvider(wrappedProvider)
      instance.setConfig({ ...instance.getConfig(), enabled: true })
      instance.enable()
      // Vitest has already imported the builtins. Patch their live exports,
      // then refresh named ESM bindings as well as default imports.
      for (const definition of instance._modules || []) {
        if (!isBuiltin(definition.name) || typeof definition.patch !== 'function') continue
        const exports = require(definition.name)
        if (definition.moduleExports !== exports) {
          definition.moduleExports = exports
          definition.patch(exports)
        }
      }
    }
    syncBuiltinESMExports()
  } catch (error) {
    warnOncePerRun(settings.spool, 'vitest-instrumentation', `Vitest HTTP instrumentation disabled (${describeError(error)}); manual children and test.execution export remain enabled.`)
  }
  const forceFlush = async () => {
    for (const forwarder of files.values()) await forwarder.shutdown()
    flush()
    // Vitest SIGTERMs forks after SDK shutdown, without Node's exit event.
    children.reportDropped()
  }
  return { forceFlush, shutdown: forceFlush }
}

// Vitest imports sdkPath in the main process before reporter initialization.
// Worker threads share its PID; forks do not. Neither needs Vitest internals.
const settings = readSettings()
module.exports = { async shutdown() {
  if (!isMainThread || process.env.VITEST_WORKER_ID) return
  const reporter = globalThis[Symbol.for('buildkite.vitest.reporter')]
  if (reporter) await reporter.shutdown()
  else warn('Vitest collector sdkPath is enabled but the collector reporter is not running. Configure buildkite-test-collector/vitest/reporter with { otelEnabled: true }; CLI --reporter flags replace config reporters (including bktec defaults). Results were not uploaded.')
} }
if (settings?.testRunner === 'vitest' && settings.native && !(isMainThread && settings.ownerPid === process.pid)) {
  try { module.exports = createWorker(settings) }
  catch (error) { warnOncePerRun(settings.spool, 'vitest-sdk', `Vitest child spans disabled (${describeError(error)}); test.execution export remains enabled.`) }
}
