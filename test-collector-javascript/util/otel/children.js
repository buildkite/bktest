const { warn } = require('./warn')
const { size, commitFile } = require('./spool')

// Child floods never displace test results. The cap includes open children and
// is per file, not per export batch. Late completions get their own commit.
const createChildExport = (settings) => {
  const cap = size('CHILD_SPAN_QUEUE_SIZE', 2048, undefined, settings.spool)
  const closedTraces = new Set()
  let spans = []
  let open = 0
  let startedLate = 0
  process.once('exit', () => {
    const reasons = [startedLate && `started after file teardown: ${startedLate}`, open && `still open when the worker exited: ${open}`].filter(Boolean)
    if (open || startedLate) warn(`OpenTelemetry dropped ${open + startedLate} child span(s) (${reasons.join(', ')}); test.execution results are unaffected.`)
  })
  const createForwarder = () => {
    const traces = new Set()
    const tracked = new Set()
    let accepted = 0
    let dropped = 0
    let active = true
    return {
      addTrace(traceId) { if (active) traces.add(traceId) },
      onStart(span) {
        if (!active || !traces.has(span.spanContext().traceId)) return false
        if (accepted >= cap) { dropped += 1; return false }
        accepted += 1
        open += 1
        tracked.add(span)
        return true
      },
      onEnd(span) {
        if (!tracked.delete(span)) return
        open -= 1
        if (active) spans.push(span)
        else {
          try { commitFile(settings.spool, undefined, { child: [span] }) } catch (error) {
            warn(`OpenTelemetry dropped 1 late child span: ${error.message}; test.execution results are unaffected. Check temporary directory permissions/free space and rerun.`)
          }
        }
      },
      shutdown: async () => {
        active = false
        for (const traceId of traces) closedTraces.add(traceId)
        traces.clear()
        if (dropped) warn(`OpenTelemetry dropped ${dropped} child span(s) (per-file cap ${cap}); test.execution results are unaffected.`)
      },
    }
  }
  return {
    createForwarder,
    take() { const ended = spans; spans = []; return ended },
    closeTrace(traceId) { closedTraces.add(traceId) },
    isClosedTrace: (traceId) => closedTraces.has(traceId),
    rejectLate(span) {
      if (!closedTraces.has(span.spanContext().traceId)) return false
      startedLate += 1
      return true
    },
  }
}

module.exports = { createChildExport }
