const { activateBuiltins } = require('../util/otel/builtins')
const Debug = require('../util/debug')

const worker = {
  enabled: new Map(),
  current: null,
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
  const provider = session.createChildProvider([dispatcher])
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
  const selected = [...instances].map(instance => worker.enabled.get(instance.instrumentationName) || instance)
  activateBuiltins(selected, worker.tracerProvider, name => worker.recording(name))
  for (const instance of selected) worker.enabled.set(instance.instrumentationName, instance)
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
