import { test, expect, beforeAll, afterAll } from 'vitest'
import peers from './peers.cjs'
import app from './app.cjs'

let running
beforeAll(async () => { running = await peers.startPeers() })
afterAll(() => running.close())

test('cjs app', async () => {
  expect(await app.work(running.ports)).toEqual({ body: 'express I/O', graphql: 'graphql I/O', redis: 'value', pg: 7 })
})
