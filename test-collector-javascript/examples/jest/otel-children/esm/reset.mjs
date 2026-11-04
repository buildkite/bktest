// Native ESM (--experimental-vm-modules), where Jest loads the API's
// CommonJS build for an import: a tracer obtained at load, before a reset in
// beforeAll, records children.
import { trace } from '@opentelemetry/api'
import http from 'node:http'

const tracer = trace.getTracer('esm')
let server
let url

beforeAll(async () => {
  import.meta.jest.resetModules()
  server = http.createServer((_request, response) => response.end('esm response'))
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  url = `http://127.0.0.1:${server.address().port}`
})
afterAll(() => new Promise((resolve) => server.close(resolve)))

test('esm after reset', async () => {
  tracer.startSpan('esm-child').end()
  await new Promise((resolve, reject) => {
    http.get(url, (response) => { response.resume(); response.on('end', resolve) }).on('error', reject)
  })
  expect(await (await fetch(url)).text()).toBe('esm response')
})
