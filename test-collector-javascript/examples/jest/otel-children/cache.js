// node-redis's client requires its own index.js while that file is still
// loading (lib/client/pool.js requires '.'), so a module of an instrumented
// package can come back unfinished. Covers redis loaded with the file and
// loaded for the first time inside a test.
const { createClient } = require('redis')
const { startFakeRedis } = require('./support/fakeRedis')

let server
beforeAll(async () => { server = await startFakeRedis() })
afterAll(() => new Promise((resolve) => server.close(resolve)))

const roundTrip = async (create) => {
  const client = create({ url: `redis://127.0.0.1:${server.address().port}` })
  await client.connect()
  await client.set('key', 'value')
  expect(await client.get('key')).toBe('value')
  await client.close()
}

test('redis loaded with the file', () => roundTrip(createClient))

test('redis loaded inside the test', async () => {
  let isolated
  jest.isolateModules(() => { isolated = require('redis') })
  await roundTrip(isolated.createClient)
})
