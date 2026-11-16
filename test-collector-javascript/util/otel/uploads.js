const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const { MAX_BYTES, MAX_RESIDENT_CHILDREN, batchSize, parseFile, restoreSpans } = require('./spool')
const { createExporter, EXPORT_TIMEOUT_MILLISECONDS } = require('./exporter')
const { warn, warnOncePerRun, describeError } = require('./warn')
const { loadSdk } = require('./sdk')

// The stock exporter exposes response data as UTF-8 text, even for binary
// google.rpc.Status. Its varint length may contain replacement characters;
// skip that framing and retain the message, not the protobuf control bytes.
const childRejectionReason = (error) => {
  const data = error?.data
  if (!data) return ''
  return String(data).match(/^(?:\x08[\x80-\uffff]*[\x00-\x7f])?\x12[\x80-\uffff]*[\x00-\x7f]([^\x00-\x1f]*)/)?.[1] || ''
}

// Only pending files are retained. Each stream has its own concurrency budget.
const createUploads = (settings, directory) => {
  const sdk = loadSdk()
  const exporterRequire = createRequire(require.resolve('@opentelemetry/exporter-trace-otlp-proto'))
  const { ProtobufTraceSerializer } = exporterRequire('@opentelemetry/otlp-transformer')
  const files = new Map()
  const acknowledged = new Set() // Only failed unlinks, to prevent replay.
  const recorded = new Map()
  const expected = new Map()
  const lanes = ['test', 'child'].map((stream) => {
    const metrics = { handed: 0, exported: 0, failed: 0 }
    return { stream, limit: batchSize(stream, directory), active: 0, metrics,
      exporter: createExporter({ sdk, ...settings, directory, stream, runKey: settings.runEnv.key, metrics }) }
  })
  let timer, completion, snapshot, childForbidden
  let heldChildren = 0
  let finalizing = false, stopped = false, spoolReadFailed = false, deadline = Infinity

  const record = (testPath, projectId) => {
    if (!testPath) return
    const key = JSON.stringify([projectId, testPath])
    const waiting = expected.get(key)
    if (waiting?.length) {
      waiting.shift()
      if (!waiting.length) expected.delete(key)
    } else recorded.set(key, (recorded.get(key) || 0) + 1)
  }

  const forget = (name) => {
    try { fs.unlinkSync(path.join(directory, name)) } catch (error) {
      acknowledged.add(name)
      warnOncePerRun(directory, 'spool-unlink', `Could not remove acknowledged OpenTelemetry spool file: ${describeError(error)}`)
    }
    files.delete(name)
    snapshot?.delete(name)
  }
  const readFailed = (error) => {
    spoolReadFailed = true
    warnOncePerRun(directory, 'spool-read', `TEST RESULTS MISSING: could not read the OpenTelemetry spool (${describeError(error)}); test and child counts are unavailable for unreadable files. Check temporary directory permissions/free space and rerun (no JSON fallback).`)
  }
  const spoolNames = () => {
    try { return fs.readdirSync(directory).filter((name) => name.endsWith('.spool')).sort() } catch (error) { readFailed(error); return [] }
  }
  const readSpoolFiles = function* (countOnly = false) {
    let childFull = false
    for (const name of countOnly ? spoolNames() : snapshot || spoolNames()) {
      if (files.has(name) || acknowledged.has(name)) continue
      const children = Number(name.match(/\.c(\d+)\.spool$/)?.[1] || 0)
      if (children && countOnly) { yield { children }; continue }
      if (children && childForbidden) continue
      if (children && (childFull || heldChildren + children > MAX_RESIDENT_CHILDREN)) { childFull = true; continue }
      try { yield { name, file: parseFile(fs.readFileSync(path.join(directory, name))) } } catch (error) { readFailed(error) }
    }
  }
  const split = (spans, stream, name) => {
    if (!spans.length) return []
    const bytes = ProtobufTraceSerializer.serializeRequest(spans).length
    if (bytes <= MAX_BYTES) return [{ spans, stream, file: name, count: spans.length, bytes, retryAt: 0, attempts: 0 }]
    if (spans.length === 1) {
      warnOncePerRun(directory, `oversize-${stream}`, `${stream === 'test' ? 'TEST RESULTS MISSING' : 'OpenTelemetry dropped a child span; test results are unaffected'}: a span exceeds 750 KiB of protobuf and cannot be uploaded; reduce its attributes and rerun (no JSON fallback).`)
      return [{ spans: stream === 'child' ? null : spans, stream, file: name, count: 1, bytes, retryAt: Infinity, oversized: true }]
    }
    const middle = Math.floor(spans.length / 2)
    return [...split(spans.slice(0, middle), stream, name), ...split(spans.slice(middle), stream, name)]
  }
  const scan = () => {
    for (const { name, file } of readSpoolFiles()) {
      try {
        const streams = restoreSpans(file.streams, sdk)
        const parts = lanes.flatMap(({ stream, limit }) => {
          const result = []
          const spans = streams[stream] || []
          for (let i = 0; i < spans.length; i += limit) result.push(...split(spans.slice(i, i + limit), stream, name))
          return result
        })
        heldChildren += parts.reduce((n, part) => n + (part.stream === 'child' && part.spans ? part.count : 0), 0)
        if (parts.length) files.set(name, { parts })
        else forget(name)
        record(file.testPath, file.projectId)
      } catch (error) {
        warnOncePerRun(directory, 'spool-decode', `TEST RESULTS MISSING: could not restore OpenTelemetry spool spans (${describeError(error)}); check the SDK installation and rerun (no JSON fallback).`)
      }
    }
  }
  const pending = () => [...files.values()].flatMap((file) => file.parts).filter((part) => !part.done)
  const arm = () => {
    if (timer || stopped || finalizing) return
    timer = setTimeout(() => { timer = null; scan(); pump() }, 1000)
    timer.unref()
  }
  const send = (lane, batch) => {
    lane.active++
    for (const part of batch) part.sending = true
    const { metrics, exporter } = lane
    const settle = (result) => {
      lane.active--
      if (stopped) return
      const success = result?.code === 0
      if (!success) lane.lastFailure = metrics.lastFailure
      const status = Number(metrics.lastFailure)
      const terminal = status >= 400 && status < 500 && status !== 429
      if (!success && lane.stream === 'child' && status === 403 && !childForbidden) {
        const message = childRejectionReason(result?.error)
        childForbidden = `HTTP 403${message ? `: ${message}` : ''}; child uploads stopped for this run`
      }
      for (const part of batch) {
        part.sending = false
        part.done = success
        if (success || (terminal && lane.stream === 'child')) {
          part.spans = null
          if (lane.stream === 'child') heldChildren -= part.count
        }
        part.attempts++
        part.retryAt = terminal ? Infinity
          : Math.max(metrics.retryAt || 0, Date.now() + Math.min(5000, 1000 * part.attempts))
      }
      if (childForbidden) {
        for (const part of pending()) {
          if (part.stream !== 'child' || part.sending || !part.spans) continue
          part.spans = null
          heldChildren -= part.count
          part.retryAt = Infinity
        }
      }
      for (const name of new Set(batch.map((part) => part.file))) {
        if (files.get(name).parts.every((part) => part.done)) forget(name)
      }
      if (finalizing && (success || terminal) && lane.stream === 'child') scan()
      pump(); arm()
    }
    try {
      exporter.export(batch.flatMap((part) => part.spans), settle)
    } catch (error) {
      metrics.lastFailure = describeError(error)
      settle({ code: 1, error })
    }
  }
  const pump = () => {
    if (stopped || Date.now() >= deadline) return
    for (const lane of lanes) {
      while (lane.active < 4 && !(lane.stream === 'child' && childForbidden)) {
        const batch = []
        let bytes = 0, count = 0
        for (const part of pending()) {
          if (part.stream !== lane.stream || part.sending || part.retryAt > Date.now()) continue
          // Summed independently encoded sizes are an upper bound on the
          // combined protobuf (shared resources/scopes can only reduce it).
          if (count + part.count > lane.limit || bytes + part.bytes > MAX_BYTES) continue
          batch.push(part); count += part.count; bytes += part.bytes
        }
        if (!batch.length) break
        send(lane, batch)
      }
    }
    if (pending().length) arm()
  }
  const collect = (testPath, tests = 0, reason = 'no runtime', projectId) => {
    if (!finalizing) arm()
    const key = JSON.stringify([projectId, testPath])
    const count = recorded.get(key) || 0
    if (count > 1) recorded.set(key, count - 1)
    else if (count) recorded.delete(key)
    else {
      if (!expected.has(key)) expected.set(key, [])
      expected.get(key).push({ tests, reason })
    }
  }
  // Snapshot once; SIGTERM may shorten a normal drain already in progress.
  const drain = (budget = EXPORT_TIMEOUT_MILLISECONDS) => {
    deadline = Math.min(deadline, Date.now() + budget)
    return completion ||= (async () => {
      finalizing = true
      clearTimeout(timer)
      snapshot = new Set(spoolNames())
      scan(); pump()
      while (pending().length && Date.now() < deadline) {
        if (pending().every((part) => part.retryAt === Infinity)) break
        await new Promise((resolve) => setTimeout(resolve, 20))
        pump()
      }
      stopped = true
      for (const lane of lanes) lane.exporter.shutdown().catch(() => {})
      const counts = { test: 0, child: 0 }
      const oversized = { test: 0, child: 0 }
      for (const part of pending()) {
        counts[part.stream] += part.count
        if (part.oversized) oversized[part.stream] += part.count
      }
      for (const { file, children } of readSpoolFiles(true)) {
        if (children) counts.child += children
        else {
          record(file.testPath, file.projectId)
          counts.test += file.streams.test?.length || 0
        }
      }
      const unacknowledgedChildren = counts.child
      const childReasons = new Set()
      try {
        for (const name of fs.readdirSync(directory).filter((name) => name.endsWith('.dropped'))) {
          const { count, reason } = JSON.parse(fs.readFileSync(path.join(directory, name), 'utf8'))
          counts.child += count
          childReasons.add(reason)
        }
      } catch (error) {
        warnOncePerRun(directory, 'child-drop-read', `OpenTelemetry child-drop total is incomplete: ${describeError(error)}; test.execution results are unaffected.`)
      }
      const missing = [...expected.values()].flat().map((file) => spoolReadFailed && file.reason === 'setup failed'
        ? { ...file, reason: 'spool unreadable' } : file)
      for (const reason of new Set(missing.map((file) => file.reason))) {
        const fix = reason === 'spool unreadable' ? 'check temporary directory permissions/free space; recording status is unknown'
          : reason === 'jasmine' ? 'use the jest-circus test runner' : reason === 'setup failed'
          ? 'fix the SDK/runtime configuration described in earlier worker warnings' : "set runtime to 'buildkite-test-collector/jest/runtime'"
        warn(`TEST RESULTS MISSING: some files have no readable OpenTelemetry commit (${reason}) and were not uploaded; ${fix}, then rerun (no JSON fallback).`)
      }
      const missingTests = missing.reduce((n, file) => n + file.tests, 0)
      const reason = (stream) => {
        // Oversized spans are never sent, so no upload failure explains them.
        if ((stream === 'child' ? unacknowledgedChildren : counts.test) === oversized[stream]) return 'span over 750 KiB'
        const lane = lanes.find((lane) => lane.stream === stream)
        const failure = lane.lastFailure || lane.metrics.lastFailure
        return /^\d+$/.test(failure) ? `HTTP ${failure}` : failure || 'drain deadline'
      }
      if (counts.test) warn(`TEST RESULTS MISSING: ${counts.test} test.execution span(s), ${counts.child} child span(s) were not acknowledged (${reason('test')}); results may not have reached Buildkite Test Engine. Check the endpoint/token/runtime and rerun; no JSON fallback.`)
      if (missing.length) {
        const reasons = [...new Set(missing.map((file) => file.reason))].map((reason) => {
          const group = missing.filter((file) => file.reason === reason)
          return `${reason}: ${group.length} file(s), ${group.reduce((n, file) => n + file.tests, 0)} test(s)`
        }).join('; ')
        warn(`TEST RESULTS MISSING: ${missing.length} file(s), ${missingTests} test(s) have no readable spool commit (${reasons}; unknown child count); fix the earlier warnings and rerun; no JSON fallback.`)
      }
      counts.test += missingTests
      if (counts.child) {
        if (childForbidden) childReasons.add(childForbidden)
        else if (unacknowledgedChildren) childReasons.add(reason('child'))
        warn(`OpenTelemetry dropped ${counts.child} child span(s) in total (${[...childReasons].join('; ')}); test.execution results are unaffected by child export failures.`)
      }
      return counts
    })()
  }
  return { collect, drain }
}

module.exports = { createUploads }
