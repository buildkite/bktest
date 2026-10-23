/**
 * @jest-environment node
 */
// Runs after io.js in the same process, outside the collector's environment:
// the collector's instrumentation must neither record this file's requests
// nor add trace headers to them.
const http = require('http')

test('opted-out request carries no trace headers', async () => {
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
