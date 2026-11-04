const { diag, DiagLogLevel } = require('@opentelemetry/api')
const selected = []
diag.setLogger({
  error() {}, warn() {}, info() {}, verbose() {},
  debug(message, names) {
    if (message === 'buildkite-test-collector: OpenTelemetry instrumentations selected for this file:') selected.push(names)
  },
}, DiagLogLevel.DEBUG)
afterAll(() => expect(selected).toEqual(['http, undici']))
