const http = require('http')
const { trace } = require('@opentelemetry/api')
const { graphql, buildSchema } = require('graphql')
// Express 4's main module exports lib/express.js unchanged, which loads
// first. The app registers its route as the file loads, before the collector
// decides who owns the provider, as an app a test file requires does.
const express = require('express')

const app = express()
app.get('/route', (req, res) => res.send('express I/O'))

// Obtained before the collector registers its provider at the first test.
const tracer = trace.getTracer('fixture')
let server
let url

beforeAll(async () => {
  // A detached trace outside any test must never be forwarded.
  tracer.startSpan('outside-test', { root: true }).end()
  server = http.createServer((req, res) => res.end('real I/O'))
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  url = `http://127.0.0.1:${server.address().port}`
})
afterAll(() => new Promise((resolve) => server.close(resolve)))

const get = (target) => new Promise((resolve, reject) => {
  http.get(target, (res) => {
    let body = ''
    res.on('data', (chunk) => { body += chunk })
    res.on('end', () => resolve(body))
  }).on('error', reject)
})

describe('per-test hooks', () => {
  beforeEach((done) => {
    setImmediate(() => { tracer.startSpan('setup-child').end(); done() })
  })
  afterEach(async () => {
    await Promise.resolve()
    tracer.startSpan('teardown-child').end()
  })

  test('http I/O', async () => {
    await tracer.startActiveSpan('manual-parent', async (span) => {
      try {
        expect(await get(url)).toBe('real I/O')
        // Without the collector's runtime, or with OpenTelemetry configured
        // outside the sandbox, nothing sets up the sandbox's API.
        if (!['preload', 'no-runtime'].includes(process.env.OTEL_CHILD_CASE)) expect(trace.getActiveSpan()).toBe(span)
      } finally {
        span.end()
      }
    })
    if (process.env.OTEL_CHILD_CASE === 'flood') {
      for (let i = 0; i < 100; i++) tracer.startSpan('flood-child').end()
    }
  })

  test('undici I/O', async () => {
    const response = await fetch(url)
    expect(await response.text()).toBe('real I/O')
  })

  test('graphql resolver I/O', async () => {
    const schema = buildSchema('type Query { message: String }')
    const result = await graphql({
      schema, source: '{ message }',
      rootValue: { message: async () => (await fetch(url)).text() },
    })
    expect(result).toEqual({ data: { message: 'real I/O' } })
  })

  test('callback body', (done) => {
    setImmediate(() => { tracer.startSpan('callback-child').end(); done() })
  })
})

test('express route I/O', async () => {
  const routeServer = await new Promise((resolve) => { const listening = app.listen(0, '127.0.0.1', () => resolve(listening)) })
  try {
    expect(await get(`http://127.0.0.1:${routeServer.address().port}/route`)).toBe('express I/O')
  } finally {
    await new Promise((resolve) => routeServer.close(resolve))
  }
})

// Different delays ensure a single mutable "current test" implementation
// misparents at least one child's continuation. Jest 27 runs .each as
// ordinary tests, and starts plain test.concurrent bodies while the file is
// still being collected.
test.concurrent.each([['slow', 35], ['fast', 5]])('concurrent %s', async (name, delay) => {
  await new Promise((resolve) => setTimeout(resolve, delay))
  tracer.startSpan(`concurrent-${name}`).end()
})

describe('plain', () => {
  test.concurrent('concurrent plain slow', async () => {
    tracer.startSpan('concurrent-plain-slow-start').end()
    await new Promise((resolve) => setTimeout(resolve, 35))
    tracer.startSpan('concurrent-plain-slow').end()
  })

  test.concurrent('concurrent plain fast', async () => {
    await new Promise((resolve) => setTimeout(resolve, 5))
    tracer.startSpan('concurrent-plain-fast').end()
  })
})
