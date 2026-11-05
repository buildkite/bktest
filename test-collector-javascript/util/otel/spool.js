const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const { randomUUID } = require('crypto')
const { warn } = require('./warn')

const MAX_BYTES = 750 * 1024
const size = (suffix, fallback, limit = 2147483647) => {
  const name = `BUILDKITE_TESTS_OTEL_${suffix}`
  const raw = process.env[name]
  if (raw === undefined) return fallback
  const value = /^[0-9]+$/.test(raw) ? Number(raw) : NaN
  if (value >= 1 && value <= limit) return value
  warn(`${name} must be a positive integer up to ${limit}, using default ${fallback}`)
  return fallback
}
const batchSize = (stream) => stream === 'test'
  ? size('TEST_SPAN_BATCH_SIZE', 120, 200) : size('CHILD_SPAN_BATCH_SIZE', 512)

// Serialize with the installed exporter's transformer, never a second OTLP
// implementation. Concatenating these requests merges their resourceSpans.
const commitFile = (directory, testPath, streams) => {
  const load = createRequire(require.resolve('@opentelemetry/exporter-trace-otlp-proto'))
  const { ProtobufTraceSerializer } = load('@opentelemetry/otlp-transformer')
  const parts = []
  for (const [stream, spans] of Object.entries(streams)) {
    const encode = (batch) => {
      const bytes = Buffer.from(ProtobufTraceSerializer.serializeRequest(batch))
      if (bytes.length <= MAX_BYTES) {
        parts.push({ stream, count: batch.length, bytes: bytes.toString('base64') })
      } else if (batch.length > 1) {
        const half = Math.floor(batch.length / 2)
        encode(batch.slice(0, half))
        encode(batch.slice(half))
      } else {
        warn(`TEST RESULTS MISSING: 1 ${stream} span was not uploaded because its encoded size exceeds 750 KiB; reduce span attributes and rerun.`)
      }
    }
    const batch = batchSize(stream)
    for (let i = 0; i < spans.length; i += batch) encode(spans.slice(i, i + batch))
  }
  const file = path.join(directory, randomUUID())
  try {
    fs.writeFileSync(`${file}.tmp`, JSON.stringify({ testPath, parts }), { mode: 0o600, flag: 'wx' })
    fs.renameSync(`${file}.tmp`, `${file}.spool`)
  } finally {
    fs.rmSync(`${file}.tmp`, { force: true })
  }
}

module.exports = { commitFile, MAX_BYTES, size, batchSize }
