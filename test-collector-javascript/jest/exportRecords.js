const { createHash } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')

// The test environment records each test file it exports over OpenTelemetry,
// and the reporter uploads JSON only for files without a record. Only the
// worker that ran a file knows for certain which environment ran it, so this
// keeps every execution on exactly one path even when the reporter cannot
// load that environment itself (Yarn Plug'n'Play, for example).
const recordPath = (dir, projectId, testPath) =>
  path.join(dir, createHash('sha1').update(`${projectId}\0${testPath}`).digest('hex'))

module.exports = {
  createRecords: () => fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-test-collector-')),
  recordExport: (dir, projectId, testPath) => fs.writeFileSync(recordPath(dir, projectId, testPath), ''),
  isExported: (dir, projectId, testPath) => fs.existsSync(recordPath(dir, projectId, testPath)),
  removeRecords: (dir) => fs.rmSync(dir, { recursive: true, force: true }),
}
