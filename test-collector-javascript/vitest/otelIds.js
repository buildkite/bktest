const { createHash } = require('crypto')
const path = require('path')

// Vitest IDs contain a 32-bit file hash: retain the full identity on both sides.
const testIds = (seed, root, project, file, id) => {
  const hash = createHash('sha256').update(JSON.stringify([seed, root, project || '', file, id])).digest('hex')
  return { traceId: hash.slice(0, 32), spanId: hash.slice(32, 48) }
}

// Workers derive IDs from Vitest's experimental span attributes, the reporter
// from its results. Workers record every ID they derive, per project, so the
// reporter can tell when a Vitest release changes those spans.
testIds.rootsFile = (spool, project) =>
  path.join(spool, `vitest-roots-${createHash('sha1').update(project || '').digest('hex')}`)

module.exports = testIds
