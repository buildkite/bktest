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
// different environments. Jest accepts two projects with the same explicit
// name (Jest 28: id), so the key is the whole project config. Jest copies it
// to workers as JSON (or a structured clone with workerThreads), which
// JSON.stringify sees the same. Only testEnvironmentOptions is left out: the
// worker merges a file's @jest-environment-options docblock into it.
const recordPath = (dir, projectConfig, testPath) => {
  const { testEnvironmentOptions, ...project } = projectConfig
  return path.join(dir, createHash('sha1').update(JSON.stringify(project)).update('\0').update(testPath).digest('hex'))
}

module.exports = {
  createRecords: () => fs.mkdtempSync(path.join(os.tmpdir(), 'buildkite-test-collector-')),
  recordExport: (dir, projectConfig, testPath) => fs.writeFileSync(recordPath(dir, projectConfig, testPath), ''),
  isExported: (dir, projectConfig, testPath) => {
    try {
      return fs.existsSync(recordPath(dir, projectConfig, testPath))
    } catch {
      return false
    }
  },
  removeRecords: (dir) => fs.rmSync(dir, { recursive: true, force: true }),
}
