const express = require('express')
const { graphql, buildSchema } = require('graphql')
const { createClient } = require('redis')
const pg = require('pg')

const schema = buildSchema('type Query { message: String }')

const work = async (ports) => {
  const app = express()
  app.get('/route', (req, res) => res.send('express I/O'))
  const server = await new Promise((resolve) => { const listening = app.listen(0, '127.0.0.1', () => resolve(listening)) })
  let body
  try { body = await (await fetch(`http://127.0.0.1:${server.address().port}/route`)).text() } finally { server.close() }
  const { data } = await graphql({ schema, source: '{ message }', rootValue: { message: async () => 'graphql I/O' } })
  const redis = createClient({ url: `redis://127.0.0.1:${ports.redis}` })
  await redis.connect()
  await redis.set('key', 'value')
  const value = await redis.get('key')
  await redis.quit()
  const client = new pg.Client({ host: '127.0.0.1', port: ports.pg, user: 'fixture', database: 'fixture', ssl: false })
  await client.connect()
  const { rows } = await client.query('SELECT 7 AS answer')
  await client.end()
  return { body, graphql: data.message, redis: value, pg: rows[0].answer }
}

module.exports = { work }
