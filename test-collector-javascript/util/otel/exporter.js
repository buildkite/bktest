const fs = require('fs')
const path = require('path')
const zlib = require('zlib')
const { MAX_BYTES, batchSize } = require('./spool')
const { warn, warnOncePerRun, describeError } = require('./warn')
const { loadSdk } = require('./sdk')

// Only the reporter owns HTTP. Each lane can have four requests in flight;
// child floods/outages cannot consume the test lane's capacity.
const createSender = (settings, directory) => {
  const { api, core } = loadSdk()
  const files = new Map()
  const acknowledged = new Set() // Only files whose unlink failed; never replay them.
  const recorded = new Map()
  const lanes = ['test', 'child'].map((stream) => ({ stream, limit: batchSize(stream), active: 0 }))
  const requests = new Set()
  let timer
  let finalizing = false
  let stopped = false
  let completion
  let lastFailure = ''

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
        try {
          result.push({ name, file: JSON.parse(fs.readFileSync(path.join(directory, name), 'utf8')) })
        } catch (error) { failed(error) }
      }
    } catch (error) { failed(error) }
    return result
  }
  const scan = () => {
    for (const { name, file } of readSpoolFiles()) {
      for (const part of file.parts) {
        part.bytes = Buffer.from(part.bytes, 'base64')
        part.retryAt = 0
        part.attempts = 0
        part.file = name
      }
      if (file.parts.length) files.set(name, file)
      else forget(name) // An empty commit has nothing to acknowledge.
      if (file.testPath) recorded.set(file.testPath, (recorded.get(file.testPath) || 0) + 1)
    }
  }
  const pending = () => [...files.values()].flatMap((file) => file.parts).filter((part) => !part.done)
  const arm = (delay = 1000) => {
    if (timer || stopped || finalizing) return
    timer = setTimeout(() => { timer = null; scan(); pump() }, delay)
    timer.unref()
  }

  const send = (lane, batch) => {
    lane.active += 1
    for (const part of batch) part.sending = true
    const body = zlib.gzipSync(Buffer.concat(batch.map((part) => part.bytes)))
    // Required in band too: a reporter request can overlap a running test.
    api.context.with(core.suppressTracing(api.ROOT_CONTEXT), () => {
      const transport = settings.endpoint.startsWith('https:') ? require('https') : require('http')
      let settled = false
      let timeout
      let request
      const finish = (status, retryAfter, error) => {
        if (settled) return
        settled = true
        clearTimeout(timeout)
        requests.delete(request)
        lane.active -= 1
        const success = status >= 200 && status < 300
        if (!success) lastFailure = error?.code || error?.message || `HTTP ${status}`
        const seconds = Number(retryAfter)
        const retryDelay = retryAfter !== undefined && Number.isFinite(seconds)
          ? Math.max(0, seconds * 1000) : Math.max(0, Date.parse(retryAfter) - Date.now())
        for (const part of batch) {
          part.sending = false
          part.done = success
          if (success) part.bytes = null
          part.attempts += 1
          // Permanent errors stay on disk and are reported, not retried.
          part.retryAt = status && status !== 429 && status < 500 ? Infinity
            : Date.now() + (Number.isFinite(retryDelay) ? Math.max(10, retryDelay) : Math.min(5000, 1000 * part.attempts))
        }
        for (const name of new Set(batch.map((part) => part.file))) {
          const file = files.get(name)
          if (file.parts.every((part) => part.done)) forget(name)
        }
        if (!stopped) { pump(); arm() }
      }
      try {
        request = transport.request(settings.endpoint, {
          method: 'POST', headers: {
            Authorization: `Token token="${settings.token}"`,
            'Buildkite-Tests-Run-Key': settings.runEnv.key,
            'Buildkite-Tests-Span-Stream': lane.stream,
            'Content-Type': 'application/x-protobuf', 'Content-Encoding': 'gzip',
            'Content-Length': body.length,
          },
        }, (response) => {
          response.resume()
          response.on('end', () => finish(response.statusCode, response.headers['retry-after']))
          response.on('error', (error) => finish(undefined, undefined, error))
        })
      } catch (error) {
        finish(undefined, undefined, error)
        return
      }
      requests.add(request)
      request.on('error', (error) => finish(undefined, undefined, error))
      timeout = setTimeout(() => request.destroy(new Error('OTLP request timeout')), 30000)
      request.end(body)
    })
  }

  const pump = () => {
    if (stopped) return
    for (const lane of lanes) {
      while (lane.active < 4) {
        const batch = []
        let bytes = 0
        let count = 0
        for (const part of pending()) {
          if (part.stream !== lane.stream || part.sending || part.retryAt > Date.now()) continue
          if (count + part.count > lane.limit || bytes + part.bytes.length > MAX_BYTES) break
          batch.push(part)
          count += part.count
          bytes += part.bytes.length
        }
        if (!batch.length) break
        send(lane, batch)
      }
    }
    if (pending().length) arm()
  }

  const collect = (testPath) => {
    if (!finalizing) { scan(); arm() }
    const count = recorded.get(testPath) || 0
    if (count > 1) recorded.set(testPath, count - 1)
    else recorded.delete(testPath)
    return count > 0
  }

  // Snapshot once, then only retry those parts. Never chase newly finishing
  // files during bail/SIGTERM or allow re-entrant onRunComplete to drain twice.
  const drain = () => completion ||= (async () => {
    finalizing = true
    clearTimeout(timer)
    scan()
    const deadline = Date.now() + 5000
    pump()
    while (pending().length && Date.now() < deadline) {
      if (pending().every((part) => part.retryAt === Infinity)) break
      await new Promise((resolve) => setTimeout(resolve, 20))
      pump()
    }
    stopped = true
    for (const request of requests) request.destroy()
    const counts = { test: 0, child: 0 }
    for (const part of pending()) counts[part.stream] += part.count
    // Report arrivals outside the upload snapshot too, without extending it.
    for (const { file } of readSpoolFiles()) {
      for (const part of file.parts) counts[part.stream] += part.count
    }
    if (counts.test || counts.child) warn(`TEST RESULTS MISSING: ${counts.test} test.execution span(s), ${counts.child} child span(s) were not acknowledged (${lastFailure || 'drain deadline'}); results may not have reached Buildkite Test Engine. Check the endpoint/token and rerun; no JSON fallback.`)
    return counts
  })()

  return { collect, drain }
}

module.exports = { createSender }
