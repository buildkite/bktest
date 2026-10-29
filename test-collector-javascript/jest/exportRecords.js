const { createHash } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')

// The test environment records each test file it exports over OpenTelemetry,
// and the reporter uploads JSON only for files without a record. Only the
// worker that ran a file knows for certain which environment ran it, so this
// keeps each execution on one path even when the reporter cannot load that
// environment itself (Yarn Plug'n'Play, for example).
//
// Records are per project, since projects can run the same file under
// different environments. Jest accepts two projects with the same explicit
// id, so the key is the whole project config. Jest copies it to workers as
// JSON (or a structured clone with workerThreads), which JSON.stringify sees
// the same. Only testEnvironmentOptions is left out: the
// worker merges a file's @jest-environment-options docblock into it.
// Projects whose configs differ only there still share a key, so each
// exporting worker adds its own numbered record, and the reporter can tell
// when fewer of them exported than ran.
const recordKey = (projectConfig, testPath) => {
  const { testEnvironmentOptions, ...project } = projectConfig
  return createHash('sha1').update(JSON.stringify(project)).update('\0').update(testPath).digest('hex')
}

const recordPath = (dir, key, index) => path.join(dir, `${key}.${index}`)

module.exports = {
  recordKey,
  createRecords: () => fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-test-collector-')),
  recordExport: (dir, key) => {
    for (let index = 0; ; index++) {
      try {
        return fs.writeFileSync(recordPath(dir, key, index), '', { flag: 'wx' })
      } catch (error) {
        if (error.code !== 'EEXIST') throw error
      }
    }
  },
  exportCount: (dir, key) => {
    let count = 0
    while (fs.existsSync(recordPath(dir, key, count))) count++
    return count
  },
  removeRecords: (dir) => fs.rmSync(dir, { recursive: true, force: true }),
}
