const { createHash } = require('crypto')

// Vitest IDs contain a 32-bit file hash: retain the full identity on both sides.
module.exports = (seed, root, project, file, id) => {
  const hash = createHash('sha256').update(JSON.stringify([seed, root, project || '', file, id])).digest('hex')
  return { traceId: hash.slice(0, 32), spanId: hash.slice(32, 48) }
}
