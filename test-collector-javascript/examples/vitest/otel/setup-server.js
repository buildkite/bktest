import { test, beforeAll, afterAll } from 'vitest'
import { get, createServer } from 'node:http'
import { tracer } from './app.js'

// The server's async context comes from beforeAll, not the test.
let server, url
beforeAll(async () => {
  server = createServer((req, res) => { tracer.startSpan('setup-server-handler').end(); res.end('ok') })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  url = `http://127.0.0.1:${server.address().port}/`
})
afterAll(async () => {
  server.closeAllConnections()
  await new Promise(resolve => server.close(resolve))
})

test('requests a server started in beforeAll', async () => {
  await new Promise((resolve, reject) => get(url, res => { res.resume(); res.on('end', resolve) }).on('error', reject))
  await (await fetch(url)).text()
})
