const fs = require('fs')
const path = require('path')
const { randomUUID } = require('crypto')
const { serialize, deserialize } = require('v8')
const { warnOncePerRun, describeError } = require('./warn')

const MAX_BYTES = 750 * 1024
const MAX_RESIDENT_CHILDREN = 16384
const size = (suffix, fallback, limit = 2147483647, directory) => {
  const name = `BUILDKITE_TESTS_OTEL_${suffix}`
  const raw = process.env[name]
  if (raw === undefined) return fallback
  const value = /^[0-9]+$/.test(raw) ? Number(raw) : NaN
  if (value >= 1 && value <= limit) return value
  warnOncePerRun(directory, name, `${name} must be a positive integer up to ${limit}, using default ${fallback}`)
  return fallback
}
const batchSize = (stream, directory) => stream === 'test'
  ? size('TEST_SPAN_BATCH_SIZE', 1000, 1000, directory) : size('CHILD_SPAN_BATCH_SIZE', 512, undefined, directory)

// Spools never leave this Node process tree. V8 preserves SDK 3's structured
// attributes (including Uint8Array and null) as well as non-finite numbers.
const parseFile = (buffer) => deserialize(buffer)

const spanData = (span) => {
  const context = (value) => value && { ...value, traceState: value.traceState?.serialize() }
  return {
    name: span.name, kind: span.kind, context: context(span.spanContext()),
    parentSpanContext: context(span.parentSpanContext),
    startTime: span.startTime, endTime: span.endTime, duration: span.duration,
    attributes: span.attributes, events: span.events,
    links: span.links.map((link) => ({ ...link, context: context(link.context) })),
    status: span.status, instrumentationScope: span.instrumentationScope,
    resource: { attributes: span.resource.attributes, schemaUrl: span.resource.schemaUrl },
    droppedAttributesCount: span.droppedAttributesCount,
    droppedEventsCount: span.droppedEventsCount, droppedLinksCount: span.droppedLinksCount,
  }
}

// Rebuild the SDK's ReadableSpan contract, preserving shared resources within
// a file so protobuf resource grouping stays unchanged.
const restoreSpans = (streams, sdk) => {
  const resources = new Map()
  const scopes = new Map()
  const context = ({ traceState, ...value }) => ({ ...value, traceState: traceState === undefined ? undefined : sdk.api.createTraceState(traceState) })
  return Object.fromEntries(Object.entries(streams).map(([stream, spans]) => [stream, spans.map((span) => {
    const key = serialize(span.resource).toString('base64')
    if (!resources.has(key)) resources.set(key, sdk.resources.resourceFromAttributes(span.resource.attributes, { schemaUrl: span.resource.schemaUrl }))
    const scopeKey = serialize(span.instrumentationScope).toString('base64')
    if (!scopes.has(scopeKey)) scopes.set(scopeKey, span.instrumentationScope)
    const spanContext = context(span.context)
    return {
      ...span, ended: true, spanContext: () => spanContext,
      parentSpanContext: span.parentSpanContext && context(span.parentSpanContext),
      links: span.links.map((link) => ({ ...link, context: context(link.context) })),
      resource: resources.get(key), instrumentationScope: scopes.get(scopeKey),
    }
  })]))
}

// Small count-only records let the parent include worker drops in its total.
const recordChildDrops = (directory, count, reason) => {
  try {
    fs.writeFileSync(path.join(directory, `${randomUUID()}.dropped`), JSON.stringify({ count, reason }), { mode: 0o600, flag: 'wx' })
    return true
  } catch (error) {
    warnOncePerRun(directory, 'child-drop-accounting', `OpenTelemetry dropped child spans (${reason}); the run total is unavailable because drop counts could not be written (${describeError(error)}); test.execution results are unaffected.`)
    return false
  }
}

// Workers only commit plain span data; the parent owns protobuf and export.
const commitFile = (directory, testPath, streams, projectId) => {
  const write = (suffix, data) => {
    const file = path.join(directory, `${Date.now()}-${randomUUID()}${suffix}`)
    try {
      fs.writeFileSync(`${file}.tmp`, serialize(data), { mode: 0o600, flag: 'wx' })
      fs.renameSync(`${file}.tmp`, `${file}.spool`)
    } finally {
      fs.rmSync(`${file}.tmp`, { force: true })
    }
  }
  // Publish children before the test commit used for file accounting. Child
  // serialization and I/O must never prevent that test commit.
  const children = streams.child || []
  for (let i = 0; i < children.length; i += MAX_RESIDENT_CHILDREN) {
    const count = Math.min(MAX_RESIDENT_CHILDREN, children.length - i)
    try {
      write(`.c${count}`, { streams: { child: children.slice(i, i + count).map(spanData) } })
    } catch (error) {
      const reason = `could not commit child spool: ${describeError(error)}`
      if (recordChildDrops(directory, count, reason)) {
        warnOncePerRun(directory, 'child-spool-write', `OpenTelemetry dropped child spans (${reason}); test.execution results are unaffected.`)
      }
    }
  }
  if (testPath !== undefined) write('', { testPath, projectId, streams: { test: (streams.test || []).map(spanData) } })
}

module.exports = { commitFile, recordChildDrops, parseFile, restoreSpans, MAX_BYTES, MAX_RESIDENT_CHILDREN, size, batchSize }
