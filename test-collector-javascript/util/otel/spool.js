const fs = require('fs')
const path = require('path')
const { randomUUID } = require('crypto')
const { warnOncePerRun } = require('./warn')

const MAX_BYTES = 750 * 1024
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
  ? size('TEST_SPAN_BATCH_SIZE', 120, 200, directory) : size('CHILD_SPAN_BATCH_SIZE', 512, undefined, directory)

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
    const key = JSON.stringify(span.resource)
    if (!resources.has(key)) resources.set(key, sdk.resources.resourceFromAttributes(span.resource.attributes, { schemaUrl: span.resource.schemaUrl }))
    const scopeKey = JSON.stringify(span.instrumentationScope)
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

// Workers only commit plain span data; the parent owns protobuf and export.
const commitFile = (directory, testPath, streams) => {
  const data = Object.fromEntries(Object.entries(streams).map(([stream, spans]) => [stream, spans.map(spanData)]))
  const file = path.join(directory, randomUUID())
  try {
    fs.writeFileSync(`${file}.tmp`, JSON.stringify({ testPath, streams: data }), { mode: 0o600, flag: 'wx' })
    fs.renameSync(`${file}.tmp`, `${file}.spool`)
  } finally {
    fs.rmSync(`${file}.tmp`, { force: true })
  }
}

module.exports = { commitFile, restoreSpans, MAX_BYTES, size, batchSize }
