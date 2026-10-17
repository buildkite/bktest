// A loopback OTLP/HTTP receiver for end-to-end tests: gunzips each request,
// decodes the ExportTraceServiceRequest protobuf into plain objects and keeps
// the headers, so tests can assert on exactly what the collector sent.
const http = require('http')
const zlib = require('zlib')

// Minimal protobuf wire-format reader; enough for the OTLP trace message.
const readVarint = (buf, pos) => {
  let result = 0n
  let shift = 0n
  for (;;) {
    const byte = buf[pos.offset++]
    result |= BigInt(byte & 0x7f) << shift
    if (!(byte & 0x80)) return result
    shift += 7n
  }
}

const readFields = (buf) => {
  const fields = []
  const pos = { offset: 0 }
  while (pos.offset < buf.length) {
    const tag = Number(readVarint(buf, pos))
    const number = tag >> 3
    const wireType = tag & 7
    let value
    switch (wireType) {
      case 0: value = readVarint(buf, pos); break
      case 1: value = buf.subarray(pos.offset, pos.offset + 8); pos.offset += 8; break
      case 2: {
        const length = Number(readVarint(buf, pos))
        value = buf.subarray(pos.offset, pos.offset + length)
        pos.offset += length
        break
      }
      case 5: value = buf.subarray(pos.offset, pos.offset + 4); pos.offset += 4; break
      default: throw new Error(`unsupported wire type ${wireType}`)
    }
    fields.push({ number, wireType, value })
  }
  return fields
}

const only = (fields, number) => fields.filter((f) => f.number === number).map((f) => f.value)
const first = (fields, number) => only(fields, number)[0]
const str = (buf) => (buf === undefined ? undefined : buf.toString('utf8'))
const hex = (buf) => (buf === undefined ? '' : buf.toString('hex'))
const fixed64 = (buf) => (buf === undefined ? undefined : buf.readBigUInt64LE(0))

const anyValue = (buf) => {
  const fields = readFields(buf)
  for (const field of fields) {
    switch (field.number) {
      case 1: return { type: 'string', value: str(field.value) }
      case 2: return { type: 'bool', value: field.value !== 0n }
      case 3: return { type: 'int', value: BigInt.asIntN(64, field.value) }
      case 4: return { type: 'double', value: field.value.readDoubleLE(0) }
      case 5: return { type: 'array', value: only(readFields(field.value), 1).map(anyValue) }
      case 6: return { type: 'kvlist', value: keyValues(only(readFields(field.value), 1)) }
      case 7: return { type: 'bytes', value: field.value }
    }
  }
  return { type: 'empty', value: undefined }
}

// KeyValue message buffers -> { key: { type, value } }
const keyValues = (kvs) => {
  const result = {}
  for (const kv of kvs) {
    const kvFields = readFields(kv)
    result[str(first(kvFields, 1))] = anyValue(first(kvFields, 2) || Buffer.alloc(0))
  }
  return result
}

const plainAttributes = (typed) => Object.fromEntries(Object.entries(typed).map(([k, v]) => [k, v.value]))

const decodeSpan = (buf) => {
  const f = readFields(buf)
  const attributes = keyValues(only(f, 9))
  return {
    traceId: hex(first(f, 1)),
    spanId: hex(first(f, 2)),
    parentSpanId: hex(first(f, 4)),
    name: str(first(f, 5)),
    kind: Number(first(f, 6) ?? 0n),
    startTimeUnixNano: fixed64(first(f, 7)),
    endTimeUnixNano: fixed64(first(f, 8)),
    attributes: plainAttributes(attributes),
    attributeTypes: Object.fromEntries(Object.entries(attributes).map(([k, v]) => [k, v.type])),
    droppedAttributesCount: Number(first(f, 10) ?? 0n),
    events: only(f, 11).map((e) => {
      const ef = readFields(e)
      return { name: str(first(ef, 2)), attributes: plainAttributes(keyValues(only(ef, 3))) }
    }),
    links: only(f, 13).map((l) => {
      const lf = readFields(l)
      return { traceId: hex(first(lf, 1)), spanId: hex(first(lf, 2)), traceState: str(first(lf, 3)) }
    }),
    status: (() => {
      const sf = first(f, 15)
      if (!sf) return { code: 0, message: undefined }
      const s = readFields(sf)
      return { code: Number(first(s, 3) ?? 0n), message: str(first(s, 2)) }
    })(),
  }
}

const decodeExportTraceServiceRequest = (buf) => {
  const resourceSpans = only(readFields(buf), 1).map((rs) => {
    const rsf = readFields(rs)
    const resourceBuf = first(rsf, 1)
    const resource = resourceBuf ? plainAttributes(keyValues(only(readFields(resourceBuf), 1))) : {}
    const scopeSpans = only(rsf, 2).map((ss) => {
      const ssf = readFields(ss)
      const scopeBuf = first(ssf, 1)
      const scope = scopeBuf ? readFields(scopeBuf) : []
      return {
        scope: { name: str(first(scope, 1)), version: str(first(scope, 2)) },
        spans: only(ssf, 2).map(decodeSpan),
      }
    })
    return { resource, scopeSpans }
  })
  return { resourceSpans }
}

// Starts the receiver. `respond(requestIndex)` may return a status code to
// force (e.g. 503 once) and defaults to 200.
const startOtlpServer = ({ respond } = {}) => new Promise((resolve) => {
  const requests = []
  const server = http.createServer((req, res) => {
    const chunks = []
    req.on('data', (chunk) => chunks.push(chunk))
    req.on('end', () => {
      const raw = Buffer.concat(chunks)
      const encoding = req.headers['content-encoding']
      let body = raw
      let decoded = null
      let decodeError = null
      try {
        if (encoding === 'gzip') body = zlib.gunzipSync(raw)
        decoded = decodeExportTraceServiceRequest(body)
      } catch (error) {
        decodeError = error
      }
      const index = requests.length
      const status = (respond && respond(index)) || 200
      requests.push({ headers: req.headers, decoded, decodeError, status })
      res.writeHead(status, { 'Content-Type': 'application/x-protobuf' })
      res.end(Buffer.alloc(0))
    })
  })
  server.listen(0, '127.0.0.1', () => {
    const { port } = server.address()
    resolve({
      url: `http://127.0.0.1:${port}/v1/traces`,
      requests,
      // Only spans the receiver accepted with a 2xx count as delivered.
      spans: () => requests.flatMap((r) => (r.decoded && r.status < 300 ? r.decoded.resourceSpans.flatMap((rs) => rs.scopeSpans.flatMap((ss) => ss.spans)) : [])),
      close: () => new Promise((done) => server.close(done)),
    })
  })
})

module.exports = { startOtlpServer }
