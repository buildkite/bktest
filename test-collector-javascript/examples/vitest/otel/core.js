import { test, beforeEach, afterEach, beforeAll, describe, expect } from 'vitest'
import { request, get, createServer } from 'node:http'
import { context, trace } from '@opentelemetry/api'
import { tracer } from './app.js'

beforeAll(async () => {
  tracer.startSpan('before-all-not-a-child').end()
  const headers = []
  const server = createServer((req, res) => {
    // happy-dom's fetch preflights cross-origin requests.
    res.setHeader('Access-Control-Allow-Origin', '*')
    if (req.method !== 'OPTIONS') headers.push(req.headers.traceparent || null)
    res.end('ok')
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const url = `http://127.0.0.1:${server.address().port}/before-all`
  try {
    // A noop tracer can still propagate an active parent outside tests.
    const parent = trace.setSpanContext(context.active(), {
      traceId: '12345678901234567890123456789012', spanId: '1234567890123456', traceFlags: 1,
    })
    await context.with(parent, async () => {
      for (const send of [request, get]) await new Promise((resolve, reject) => {
        const req = send(url, res => { res.resume(); res.on('end', resolve) })
        req.on('error', reject); req.end()
      })
      await (await fetch(url)).text()
    })
    console.log('BEFORE ALL TRACEPARENTS', JSON.stringify(headers))
  } finally {
    server.closeAllConnections()
    await new Promise(resolve => server.close(resolve))
  }
})
beforeEach((context) => {
  if (context.task.name === 'hook failure') throw new Error('intentional hook failure')
  tracer.startSpan('before-hook').end()
})
afterEach((context) => {
  if (context.task.name === 'concurrent A') trace.getActiveSpan().addEvent('cleanup-note')
  else tracer.startSpan('after-hook').end()
})

test('body and IO', async () => {
  tracer.startSpan('manual').end()
  trace.getTracer('vitest').startSpan('db.connect', { attributes: { 'db.system.name': 'postgres' } })
    .addEvent('connecting').end()
  const server = createServer((req, res) => { tracer.startSpan('handler').end(); res.end('ok') })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const url = `http://127.0.0.1:${server.address().port}/hello`
  try {
    await new Promise((resolve, reject) => {
      const req = request(url, res => { res.resume(); res.on('end', resolve) })
      req.on('error', reject); req.end()
    })
    expect(await (await fetch(url)).text()).toBe('ok')
  } finally {
    server.closeAllConnections()
    await new Promise(resolve => server.close(resolve))
  }
})
test('hook failure', () => {})
let attempts = 0
test('retry', { retry: 1 }, () => {
  tracer.startSpan('attempt').end()
  expect(++attempts).toBe(2)
})
for (const name of ['concurrent A', 'concurrent B']) test.concurrent(name, async () => {
  await new Promise(resolve => setTimeout(resolve, name.endsWith('A') ? 15 : 5))
  tracer.startSpan(name).end()
})
describe.skip('skipped suite', () => {
  test('skipped test', () => {})
  test.todo('todo test')
})
