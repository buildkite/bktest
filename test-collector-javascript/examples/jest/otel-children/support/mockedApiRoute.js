const http = require('http')
const express = require('express')

module.exports = () => {
  const app = express()
  app.get('/', (req, res) => res.send('unmodified route'))
  test('Express works with a mocked OpenTelemetry API', async () => {
    const server = await new Promise((resolve) => { const listening = app.listen(0, '127.0.0.1', () => resolve(listening)) })
    try {
      const response = await new Promise((resolve, reject) => {
        http.get(`http://127.0.0.1:${server.address().port}/`, (res) => {
          let body = ''
          res.on('data', (chunk) => { body += chunk })
          res.on('end', () => resolve({ status: res.statusCode, body }))
        }).on('error', reject)
      })
      expect(response).toEqual({ status: 200, body: 'unmodified route' })
    } finally { await new Promise((resolve) => server.close(resolve)) }
  })
}
