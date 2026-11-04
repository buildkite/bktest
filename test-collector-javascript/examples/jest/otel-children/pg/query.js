const net = require('net')
const { Client } = require('pg')

// A deterministic PostgreSQL wire peer, not a database. Exercise the real
// pg client, socket and parser without requiring a database service in CI.
const integer = (value, bytes = 4) => {
  const buffer = Buffer.alloc(bytes)
  buffer.writeIntBE(value, 0, bytes)
  return buffer
}
const message = (type, ...parts) => {
  const body = Buffer.concat(parts)
  return Buffer.concat([Buffer.from(type), integer(body.length + 4), body])
}

test('pg connect and query', async () => {
  const queries = []
  const server = net.createServer((socket) => {
    let pending = Buffer.alloc(0)
    let started = false
    socket.on('data', (chunk) => {
      pending = Buffer.concat([pending, chunk])
      if (!started) {
        if (pending.length < 4 || pending.length < pending.readInt32BE(0)) return
        pending = pending.subarray(pending.readInt32BE(0))
        started = true
        socket.write(Buffer.concat([message('R', integer(0)), message('Z', Buffer.from('I'))]))
      }
      while (pending.length >= 5 && pending.length >= pending.readInt32BE(1) + 1) {
        const size = pending.readInt32BE(1) + 1
        const type = String.fromCharCode(pending[0])
        if (type === 'Q') {
          queries.push(pending.subarray(5, size - 1).toString())
          socket.write(Buffer.concat([
            message('T', integer(1, 2), Buffer.from('answer\0'), integer(0), integer(0, 2), integer(23), integer(4, 2), integer(-1), integer(0, 2)),
            message('D', integer(1, 2), integer(1), Buffer.from('7')),
            message('C', Buffer.from('SELECT 1\0')), message('Z', Buffer.from('I')),
          ]))
        } else if (type === 'X') socket.end()
        pending = pending.subarray(size)
      }
    })
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const client = new Client({ host: '127.0.0.1', port: server.address().port, user: 'fixture', database: 'fixture', ssl: false })
  try {
    await client.connect()
    expect((await client.query('SELECT 7 AS answer')).rows).toEqual([{ answer: 7 }])
    expect(queries).toEqual(['SELECT 7 AS answer'])
  } finally {
    await client.end()
    await new Promise((resolve) => server.close(resolve))
  }
})
