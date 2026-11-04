const { createHash } = require('crypto')
const fs = require('fs')
const path = require('path')

const PREFIX = '[buildkite-test-collector]'

const warn = (message) => {
  console.warn(`${PREFIX} ${message}`)
}

const describeError = (error) => {
  if (error instanceof Error) return `${error.name}: ${error.message}`
  return String(error)
}

// Exclusive creation deduplicates warnings across worker processes.
const warned = new Set()
const warnOncePerRun = (directory, key, message) => {
  if (warned.has(key)) return
  warned.add(key)
  if (directory) {
    try {
      fs.writeFileSync(path.join(directory, `warned-${createHash('sha1').update(key).digest('hex')}`), '', { flag: 'wx' })
    } catch (error) {
      if (error.code === 'EEXIST') return
    }
  }
  warn(message)
}

module.exports = { warn, warnOncePerRun, describeError, PREFIX }
