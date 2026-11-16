import express, { type Request, type Response } from 'express'
import { graphql } from 'graphql'
import { buildSchema } from 'graphql/utilities'
import { createClient } from 'redis'
import pg from 'pg'
import type { AddressInfo } from 'node:net'

interface Ports { pg: number, redis: number }

const schema = buildSchema('type Query { message: String }')

export const work = async (ports: Ports) => {
  const app = express()
  app.get('/route', (req: Request, res: Response) => res.send('express I/O'))
  const server = await new Promise<ReturnType<typeof app.listen>>((resolve) => { const listening = app.listen(0, '127.0.0.1', () => resolve(listening)) })
  let body: string
  try { body = await (await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/route`)).text() } finally { server.close() }
  const { data } = await graphql({ schema, source: '{ message }', rootValue: { message: async () => 'graphql I/O' } })
  const redis = createClient({ url: `redis://127.0.0.1:${ports.redis}` })
  await redis.connect()
  await redis.set('key', 'value')
  const value = await redis.get('key')
  await redis.quit()
  const client = new pg.Client({ host: '127.0.0.1', port: ports.pg, user: 'fixture', database: 'fixture', ssl: false })
  await client.connect()
  const { rows } = await client.query<{ answer: number }>('SELECT 7 AS answer')
  await client.end()
  return { body, graphql: data?.message, redis: value, pg: rows[0].answer }
}
