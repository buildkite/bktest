import { test, beforeEach, afterEach, beforeAll, describe, expect } from 'vitest'
import { request, createServer } from 'node:http'
import { trace } from '@opentelemetry/api'
import { tracer } from './app.js'

beforeAll(() => tracer.startSpan('before-all-not-a-child').end())
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
