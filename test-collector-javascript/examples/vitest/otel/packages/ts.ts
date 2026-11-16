import { test, expect, beforeAll, afterAll } from 'vitest'
import peers from './peers.cjs'
import { work } from './app.ts'

let running: Awaited<ReturnType<typeof peers.startPeers>>
beforeAll(async () => { running = await peers.startPeers() })
afterAll(() => running.close())

test('ts app', async () => {
  expect(await work(running.ports)).toEqual({ body: 'express I/O', graphql: 'graphql I/O', redis: 'value', pg: 7 })
})
