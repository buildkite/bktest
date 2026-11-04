const { createHash } = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')

// Only workers know which environment actually exported a file. Key their
// records by config, not project ID (Jest allows duplicates). Exclude options
// rewritten by docblocks; numbered records detect ambiguous shared keys.
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
