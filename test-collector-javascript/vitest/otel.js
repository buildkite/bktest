const { isMainThread } = require('worker_threads')
const { isBuiltin, syncBuiltinESMExports } = require('module')
const { readSettings } = require('../util/otel/config')
const { warn, warnOncePerRun, describeError } = require('../util/otel/warn')

const createWorker = (settings) => {
  const { api, trace, resources, core } = require('../util/otel/sdk').loadSdk()
  const { idGenerator, SPAN_LIMITS } = require('../util/otel/session')
  const { resourceAttributes } = require('../util/otel/attributes')
  const { createChildExport } = require('../util/otel/children')
  const { commitFile } = require('../util/otel/spool')
  const testIds = require('./otelIds')
  const children = createChildExport(settings)
  const traces = new Set()
  const roots = new Map()
  const nodes = new Map()
  const files = new Map()
  let primed

  const flush = () => {
    const spans = children.take()
    try { commitFile(settings.spool, undefined, { child: spans }) }
    catch (error) { warn(`OpenTelemetry dropped ${spans.length} child span(s): ${describeError(error)}; test.execution results are unaffected.`) }
  }

  // Admit ancestors only when a test-owned span needs them. Empty Vitest
  // phases never consume the per-file child budget.
  const forward = (node) => {
    if (!node || node.root || node.sent !== undefined) return
    forward(node.parent)
    let parent = node.parent
    while (parent && !parent.root && !parent.sent) parent = parent.parent
    node.span.parentSpanContext = parent?.span.spanContext()
    node.sent = node.forwarder.onStart(node.span)
    if (node.ended) node.forwarder.onEnd(node.span)
  }
  const processor = {
    onStart(span) {
      const context = span.spanContext()
      if (children.rejectLate(span)) return
      const root = roots.get(context.traceId)
      if (!root) return
      const parent = nodes.get(span.parentSpanContext?.spanId) || root
      const node = { span, parent, forwarder: root.forwarder, root: context.spanId === root.id }
      nodes.set(context.spanId, node)
      root.nodes.push(context.spanId)
      if (!node.root && span.instrumentationScope.name !== 'vitest') forward(node)
    },
    onEnd(span) {
      const context = span.spanContext()
      const node = nodes.get(context.spanId)
      if (!node) return
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
  const provider = new trace.BasicTracerProvider({
    resource: resources.defaultResource()
      .merge(resources.detectResources({ detectors: [resources.envDetector] }))
      .merge(resources.resourceFromAttributes(resourceAttributes(settings.runEnv))),
    spanLimits: SPAN_LIMITS,
    idGenerator: {
      generateTraceId: () => primed?.traceId || idGenerator.generateTraceId(),
      generateSpanId: () => primed?.spanId || idGenerator.generateSpanId(),
    },
    sampler: { shouldSample: (_context, id) => ({ decision: traces.has(id)
      ? trace.SamplingDecision.RECORD_AND_SAMPLED : trace.SamplingDecision.NOT_RECORD }) },
    spanProcessors: [processor],
  })
  const wrappedProvider = { getTracer(...args) {
    const tracer = provider.getTracer(...args)
    if (args[0] !== 'vitest') return tracer
    const wrapped = Object.create(tracer)
    wrapped.startSpan = (name, options, context) => {
      const id = options?.attributes?.['vitest.test.id']
      if (id === undefined) return tracer.startSpan(name, options, context)
      const file = options.attributes['code.file.path']
      if (!files.has(file)) files.set(file, children.createForwarder())
      const forwarder = files.get(file)
      primed = testIds(settings.seed, id)
      traces.add(primed.traceId)
      forwarder.addTrace(primed.traceId)
      roots.set(primed.traceId, { id: primed.spanId, forwarder, nodes: [] })
      try { return tracer.startSpan(name, { ...options, root: true }, api.ROOT_CONTEXT) }
      finally { primed = null }
    }
    return wrapped
  } }
  const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
  const manager = new AsyncLocalStorageContextManager().enable()
  if (!api.trace.setGlobalTracerProvider(wrappedProvider)) {
    manager.disable()
    warnOncePerRun(settings.spool, 'vitest-provider', 'Vitest child spans are disabled because a tracer provider is already registered; let the collector own sdkPath. test.execution export remains enabled.')
    return { async shutdown() {} }
  }
  api.context.setGlobalContextManager(manager)
  api.propagation.setGlobalPropagator(new core.CompositePropagator({ propagators: [
    new core.W3CTraceContextPropagator(), new core.W3CBaggagePropagator(),
  ] }))
  try {
    const { loadInstrumentations } = require('../util/otel/instrumentations')
    for (const { name, instance } of loadInstrumentations(settings.rootDir,
      (key, message) => warnOncePerRun(settings.spool, key, message))) {
      if (!['http', 'undici'].includes(name)) continue
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
  }
  return { forceFlush, shutdown: forceFlush }
}

// Vitest imports sdkPath in the main process before reporter initialization.
// Worker threads share its PID; forks do not. Neither needs Vitest internals.
const settings = readSettings()
module.exports = { async shutdown() {
  if (isMainThread && !process.env.VITEST_WORKER_ID && !globalThis[Symbol.for('buildkite.vitest.reporter')]) warn('Vitest collector sdkPath is enabled but the collector reporter is not running. Configure buildkite-test-collector/vitest/reporter with { otelEnabled: true }; CLI --reporter flags replace config reporters (including bktec defaults). Results were not uploaded.')
} }
if (settings?.testRunner === 'vitest' && !(isMainThread && settings.ownerPid === process.pid)) {
  try { module.exports = createWorker(settings) }
  catch (error) { warnOncePerRun(settings.spool, 'vitest-sdk', `Vitest child spans disabled (${describeError(error)}); test.execution export remains enabled.`) }
}
