const { createHash } = require('crypto')

// TestCase.id is also Vitest's vitest.test.id; no worker/main IPC is needed.
module.exports = (seed, id) => {
  const hash = createHash('sha256').update(`${seed}:${id}`).digest('hex')
  return { traceId: hash.slice(0, 32), spanId: hash.slice(32, 48) }
}
