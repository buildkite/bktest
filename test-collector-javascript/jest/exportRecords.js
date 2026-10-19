const { createHash } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')

// The test environment records each test file it exports over OpenTelemetry,
// and the reporter uploads JSON only for files without a record. Only the
// worker that ran a file knows for certain which environment ran it, so this
// keeps every execution on exactly one path even when the reporter cannot
// load that environment itself (Yarn Plug'n'Play, for example).
//
// Records are per project, since projects can run the same file under
// different environments. Jest 28 renamed the project config's `name` to `id`;
// without either, nothing is recorded and the file goes to JSON.
const recordPath = (dir, projectConfig, testPath) => {
  const project = projectConfig.id ?? projectConfig.name
  if (typeof project !== 'string' || project === '') return null
  return path.join(dir, createHash('sha1').update(`${project}\0${testPath}`).digest('hex'))
}

module.exports = {
  createRecords: () => fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-test-collector-')),
  recordExport: (dir, projectConfig, testPath) => {
    const file = recordPath(dir, projectConfig, testPath)
    if (!file) throw new Error('the Jest project config has neither an id nor a name')
    fs.writeFileSync(file, '')
  },
  isExported: (dir, projectConfig, testPath) => {
    const file = recordPath(dir, projectConfig, testPath)
    return file !== null && fs.existsSync(file)
  },
  removeRecords: (dir) => fs.rmSync(dir, { recursive: true, force: true }),
}
