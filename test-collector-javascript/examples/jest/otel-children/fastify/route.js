const fastify = require('fastify')

test('default Fastify route', async () => {
  const app = fastify()
  app.get('/answer', async () => ({ answer: 7 }))
  try {
    const address = await app.listen({ port: 0, host: '127.0.0.1' })
    expect(await (await fetch(`${address}/answer`)).json()).toEqual({ answer: 7 })
  } finally {
    await app.close()
  }
})
