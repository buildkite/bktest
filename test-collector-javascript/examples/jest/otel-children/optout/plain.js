/**
 * @jest-environment node
 */
// Runs after io.js in the same process, outside the collector's environment:
// the built-in modules the collector patched for io.js were unpatched at its
// teardown, so this file's requests are neither recorded nor given trace
// headers.
const http = require('http')

test('opted-out request carries no trace headers', async () => {
  expect([http.request, http.get, http.Server.prototype.emit].map((fn) => fn.__wrapped)).toEqual([undefined, undefined, undefined])
  let headers
  const server = http.createServer((req, res) => { headers = req.headers; res.end('plain') })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  try {
    await new Promise((resolve, reject) => {
      http.get(`http://127.0.0.1:${server.address().port}`, (res) => { res.resume(); res.on('end', resolve) }).on('error', reject)
    })
  } finally {
    await new Promise((resolve) => server.close(resolve))
  }
  expect(headers.traceparent).toBeUndefined()
})
