const nock = require('nock')
const http = require('http')

module.exports = (name, restore = true) => {
  if (restore) afterAll(() => nock.restore())
  test(name, async () => {
    for (const protocol of ['http', 'https']) {
      const scope = nock(`${protocol}://fixture.invalid`).get('/').reply(200, function () {
        return this.req.headers.traceparent || 'missing'
      })
      const body = await new Promise((resolve, reject) => {
        require(protocol).get(`${protocol}://fixture.invalid/`, (res) => {
          let body = ''
          res.on('data', (chunk) => { body += chunk })
          res.on('end', () => resolve(body))
        }).on('error', reject)
      })
      expect(body).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/)
      expect(scope.isDone()).toBe(true)
    }
    // Nock can retain an earlier collector wrapper beneath its own. A real
    // passthrough request must still have exactly one client span.
    const server = http.createServer((req, res) => res.end(req.headers.traceparent || 'missing'))
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    try {
      const body = await new Promise((resolve, reject) => {
        http.get(`http://127.0.0.1:${server.address().port}/`, (res) => {
          let body = ''
          res.on('data', (chunk) => { body += chunk })
          res.on('end', () => resolve(body))
        }).on('error', reject)
      })
      expect(body).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/)
    } finally { await new Promise((resolve) => server.close(resolve)) }
  })
}
