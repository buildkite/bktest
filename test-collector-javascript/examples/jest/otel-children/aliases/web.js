const http = require('http')
const express = require('@fixture/web')

const app = express()
app.get('/alias', (req, res) => res.send('mapped express'))

test('mapped Express route', async () => {
  expect(express).toBe(require('express'))
  const server = await new Promise((resolve) => { const listening = app.listen(0, '127.0.0.1', () => resolve(listening)) })
  try {
    const body = await new Promise((resolve, reject) => {
      http.get(`http://127.0.0.1:${server.address().port}/alias`, (res) => {
        let body = ''
        res.on('data', (chunk) => { body += chunk })
        res.on('end', () => resolve(body))
      }).on('error', reject)
    })
    expect(body).toBe('mapped express')
  } finally { await new Promise((resolve) => server.close(resolve)) }
})
