const nock = require('nock')

module.exports = (name) => {
  afterAll(() => nock.restore())
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
  })
}
