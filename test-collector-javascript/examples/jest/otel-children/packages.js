const http = require('http')
const express = require('express')
const { graphql, buildSchema } = require('graphql')

// Register at file load, not inside the test: patching only on test_start
// misses these middleware and handler wrappers.
const app = express()
app.get('/route', (req, res) => res.send('express I/O'))
const schema = buildSchema('type Query { message: String }')

test('express route I/O', async () => {
  const server = await new Promise((resolve) => { const listening = app.listen(0, '127.0.0.1', () => resolve(listening)) })
  try {
    const body = await new Promise((resolve, reject) => {
      http.get(`http://127.0.0.1:${server.address().port}/route`, (res) => {
        let body = ''
        res.on('data', (chunk) => { body += chunk })
        res.on('end', () => resolve(body))
      }).on('error', reject)
    })
    expect(body).toBe('express I/O')
  } finally {
    await new Promise((resolve) => server.close(resolve))
  }
})

test('graphql resolver I/O', async () => {
  expect(await graphql({ schema, source: '{ message }', rootValue: { message: async () => 'graphql I/O' } }))
    .toEqual({ data: { message: 'graphql I/O' } })
})
