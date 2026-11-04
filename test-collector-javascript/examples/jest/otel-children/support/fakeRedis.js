// A minimal Redis protocol server for the cache fixture: GET returns
// "value", every other command +OK. Enough for node-redis to connect, SET
// and GET without a Redis installation.
const net = require('net')

const commands = (buffer) => {
  const found = []
  let offset = 0
  for (;;) {
    if (buffer[offset] !== 0x2a) break // '*'
    const header = buffer.indexOf('\r\n', offset)
    if (header === -1) break
    const count = Number(buffer.slice(offset + 1, header))
    let cursor = header + 2
    const args = []
    for (let i = 0; i < count; i++) {
      const lengthEnd = buffer.indexOf('\r\n', cursor)
      if (lengthEnd === -1) return { found, rest: buffer.slice(offset) }
      const length = Number(buffer.slice(cursor + 1, lengthEnd))
      if (buffer.length < lengthEnd + 2 + length + 2) return { found, rest: buffer.slice(offset) }
      args.push(buffer.slice(lengthEnd + 2, lengthEnd + 2 + length).toString())
      cursor = lengthEnd + 2 + length + 2
    }
    found.push(args)
    offset = cursor
  }
  return { found, rest: buffer.slice(offset) }
}

const startFakeRedis = () => new Promise((resolve) => {
  const server = net.createServer((socket) => {
    let pending = Buffer.alloc(0)
    socket.on('data', (data) => {
      const { found, rest } = commands(Buffer.concat([pending, data]))
      pending = rest
      for (const [name] of found) socket.write(name.toUpperCase() === 'GET' ? '$5\r\nvalue\r\n' : '+OK\r\n')
    })
  })
  server.listen(0, '127.0.0.1', () => resolve(server))
})

module.exports = { startFakeRedis }
