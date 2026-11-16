import { test, expect, beforeAll, afterAll } from 'vitest'
import peers from './peers.cjs'
import { work } from './app.mjs'

let running
beforeAll(async () => { running = await peers.startPeers() })
afterAll(() => running.close())

test('esm app', async () => {
  expect(await work(running.ports)).toEqual({ body: 'express I/O', graphql: 'graphql I/O', redis: 'value', pg: 7 })
})

test('esm failure after I/O', async () => {
  await work(running.ports)
  expect('instrumented').toBe('unchanged')
})

test('package namespaces', async () => {
  const names = await Promise.all(['express', 'graphql', 'pg', 'redis'].map(async (name) => [name, Object.keys(await import(name)).sort()]))
  console.log('NAMESPACES', JSON.stringify(names))
})
