const PREFIX = '[buildkite-test-collector]'

const warn = (message) => {
  console.warn(`${PREFIX} ${message}`)
}

const describeError = (error) => {
  if (error instanceof Error) return `${error.name}: ${error.message}`
  return String(error)
}

module.exports = { warn, describeError }
