const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const { MAX_BYTES, batchSize, restoreSpans } = require('./spool')
const { createExporter } = require('./exporter')
const { warn, warnOncePerRun, describeError } = require('./warn')
const { loadSdk } = require('./sdk')

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
  let timer, completion, childWarning
  let finalizing = false, stopped = false, deadline = Infinity

  const record = (testPath) => {
    if (!testPath) return
    const waiting = expected.get(testPath)
    if (waiting?.length) {
      waiting.shift()
      if (!waiting.length) expected.delete(testPath)
    } else recorded.set(testPath, (recorded.get(testPath) || 0) + 1)
  }

  const forget = (name) => {
    try { fs.unlinkSync(path.join(directory, name)) } catch (error) {
      acknowledged.add(name)
      warnOncePerRun(directory, 'spool-unlink', `Could not remove acknowledged OpenTelemetry spool file: ${describeError(error)}`)
    }
    files.delete(name)
  }
  const readSpoolFiles = () => {
    const result = []
    const failed = (error) => warnOncePerRun(directory, 'spool-read', `TEST RESULTS MISSING: could not read the OpenTelemetry spool (${describeError(error)}); test and child counts are unavailable for unreadable files. Check temporary directory permissions/free space and rerun (no JSON fallback).`)
    try {
      for (const name of fs.readdirSync(directory)) {
        if (!name.endsWith('.spool') || files.has(name) || acknowledged.has(name)) continue
        try { result.push({ name, file: JSON.parse(fs.readFileSync(path.join(directory, name), 'utf8')) }) } catch (error) { failed(error) }
      }
    } catch (error) { failed(error) }
    return result
  }
  const split = (spans, stream, name) => {
    if (!spans.length) return []
    const bytes = ProtobufTraceSerializer.serializeRequest(spans).length
    if (bytes <= MAX_BYTES) return [{ spans, stream, file: name, count: spans.length, bytes, retryAt: 0, attempts: 0 }]
    if (spans.length === 1) {
      warnOncePerRun(directory, `oversize-${stream}`, `${stream === 'test' ? 'TEST RESULTS MISSING' : 'OpenTelemetry dropped a child span; test results are unaffected'}: a span exceeds 750 KiB of protobuf and cannot be uploaded; reduce its attributes and rerun (no JSON fallback).`)
      return [{ spans, stream, file: name, count: 1, bytes, retryAt: Infinity }]
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
        if (parts.length) files.set(name, { parts })
        else forget(name)
        record(file.testPath)
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
      for (const part of batch) {
        part.sending = false
        part.done = success
        if (success) part.spans = null
        part.attempts++
        part.retryAt = status >= 400 && status < 500 && status !== 429 ? Infinity
          : Math.max(metrics.retryAt || 0, Date.now() + Math.min(5000, 1000 * part.attempts))
      }
      if (!success && lane.stream === 'child' && status >= 400 && status < 500 && status !== 429 && !childWarning) {
        childWarning = true
        warn(`OpenTelemetry dropped ${batch.reduce((n, part) => n + part.count, 0)} child span(s) (HTTP ${status}); test.execution results are unaffected. The run total will be reported at completion.`)
      }
      for (const name of new Set(batch.map((part) => part.file))) {
        if (files.get(name).parts.every((part) => part.done)) forget(name)
      }
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
      while (lane.active < 4) {
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
  const collect = (testPath, tests = 0, reason = 'no runtime') => {
    if (!finalizing) arm()
    const count = recorded.get(testPath) || 0
    if (count > 1) recorded.set(testPath, count - 1)
    else if (count) recorded.delete(testPath)
    else {
      if (!expected.has(testPath)) expected.set(testPath, [])
      expected.get(testPath).push({ tests, reason })
    }
  }
  // Snapshot once; SIGTERM may shorten a normal drain already in progress.
  const drain = (budget = 30000) => {
    deadline = Math.min(deadline, Date.now() + budget)
    return completion ||= (async () => {
      finalizing = true
      clearTimeout(timer)
      scan(); pump()
      while (pending().length && Date.now() < deadline) {
        if (pending().every((part) => part.retryAt === Infinity)) break
        await new Promise((resolve) => setTimeout(resolve, 20))
        pump()
      }
      stopped = true
      for (const lane of lanes) lane.exporter.shutdown().catch(() => {})
      const counts = { test: 0, child: 0 }
      for (const part of pending()) counts[part.stream] += part.count
      for (const { file } of readSpoolFiles()) {
        record(file.testPath)
        for (const stream of ['test', 'child']) counts[stream] += file.streams[stream]?.length || 0
      }
      const missing = [...expected.values()].flat()
      for (const reason of new Set(missing.map((file) => file.reason))) {
        const fix = reason === 'jasmine' ? 'use the jest-circus test runner' : reason === 'setup failed'
          ? 'fix the SDK/runtime configuration described in earlier worker warnings' : "set runtime to 'buildkite-test-collector/jest/runtime'"
        warn(`TEST RESULTS MISSING: some files did not record OpenTelemetry results (${reason}) and were not uploaded; ${fix}, then rerun (no JSON fallback).`)
      }
      const missingTests = missing.reduce((n, file) => n + file.tests, 0)
      const lane = lanes[counts.test ? 0 : 1]
      const failure = lane.lastFailure || lane.metrics.lastFailure
      const reason = /^\d+$/.test(failure) ? `HTTP ${failure}` : failure || 'drain deadline'
      if (counts.test) warn(`TEST RESULTS MISSING: ${counts.test} test.execution span(s), ${counts.child} child span(s) were not acknowledged (${reason}); results may not have reached Buildkite Test Engine. Check the endpoint/token/runtime and rerun; no JSON fallback.`)
      if (missing.length) {
        const reasons = [...new Set(missing.map((file) => file.reason))].map((reason) => {
          const group = missing.filter((file) => file.reason === reason)
          return `${reason}: ${group.length} file(s), ${group.reduce((n, file) => n + file.tests, 0)} test(s)`
        }).join('; ')
        warn(`TEST RESULTS MISSING: ${missing.length} file(s), ${missingTests} test(s) never recorded spans (${reasons}; unknown child count); fix the earlier warnings and rerun; no JSON fallback.`)
      }
      counts.test += missingTests
      if (counts.child) warn(`OpenTelemetry dropped ${counts.child} child span(s) in total (${reason}); test.execution results are unaffected by child export failures.`)
      return counts
    })()
  }
  return { collect, drain }
}

module.exports = { createUploads }
