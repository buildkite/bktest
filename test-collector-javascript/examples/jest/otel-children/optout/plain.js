/**
 * @jest-environment node
 */
// Runs after io.js in the same process, in a project without the runtime:
// worker-wide wrappers remain, but their closed gate must neither record
// requests nor inject trace headers.
const http = require('http')

test('opted-out request carries no trace headers', async () => {
  for (const mod of [http, require('https')]) for (const method of ['get', 'request']) {
    expect(() => mod[method]('/relative').on('error', () => {}).destroy()).toThrow(expect.objectContaining({ code: 'ERR_INVALID_URL' }))
  }
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
