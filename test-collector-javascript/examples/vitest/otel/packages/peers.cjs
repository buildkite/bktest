// Deterministic PostgreSQL and Redis wire peers, not databases. They exercise
// the real pg and node-redis clients without database services in CI.
const net = require('net')

const integer = (value, bytes = 4) => {
  const buffer = Buffer.alloc(bytes)
  buffer.writeIntBE(value, 0, bytes)
  return buffer
}
const message = (type, ...parts) => {
  const body = Buffer.concat(parts)
  return Buffer.concat([Buffer.from(type), integer(body.length + 4), body])
}

const postgres = (socket) => {
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
        socket.write(Buffer.concat([
          message('T', integer(1, 2), Buffer.from('answer\0'), integer(0), integer(0, 2), integer(23), integer(4, 2), integer(-1), integer(0, 2)),
          message('D', integer(1, 2), integer(1), Buffer.from('7')),
          message('C', Buffer.from('SELECT 1\0')), message('Z', Buffer.from('I')),
        ]))
      } else if (type === 'X') socket.end()
      pending = pending.subarray(size)
    }
  })
}

// GET returns "value"; every other command replies +OK.
const redis = (socket) => {
  let pending = Buffer.alloc(0)
  socket.on('data', (chunk) => {
    pending = Buffer.concat([pending, chunk])
    for (;;) {
      if (pending[0] !== 0x2a) return
      let cursor = pending.indexOf('\r\n')
      if (cursor === -1) return
      const args = []
      for (let count = Number(pending.subarray(1, cursor)); args.length < count;) {
        const end = pending.indexOf('\r\n', cursor + 2)
        if (end === -1) return
        const length = Number(pending.subarray(cursor + 3, end))
        if (pending.length < end + 2 + length + 2) return
        args.push(pending.subarray(end + 2, end + 2 + length).toString())
        cursor = end + 2 + length
      }
      pending = pending.subarray(cursor + 2)
      socket.write(args[0].toUpperCase() === 'GET' ? '$5\r\nvalue\r\n' : '+OK\r\n')
    }
  })
}

const listen = (handler) => new Promise((resolve) => {
  const server = net.createServer(handler)
  server.listen(0, '127.0.0.1', () => resolve(server))
})

const startPeers = async () => {
  const servers = await Promise.all([listen(postgres), listen(redis)])
  return {
    ports: { pg: servers[0].address().port, redis: servers[1].address().port },
    close: () => Promise.all(servers.map((server) => new Promise((resolve) => server.close(resolve)))),
  }
}

module.exports = { startPeers }
