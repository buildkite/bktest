const PREFIX = '[buildkite-test-collector]'

let handler = (message) => console.warn(message)

// Some runner hosts filter stderr; let their adapter choose a visible sink.
const setWarningHandler = (output) => { handler = output }

const warn = (message) => {
  handler(`${PREFIX} ${message}`)
}

const describeError = (error) => {
  if (error instanceof Error) return `${error.name}: ${error.message}`
  return String(error)
}

module.exports = { warn, describeError, setWarningHandler }
