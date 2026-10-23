// For suites that register their own OpenTelemetry tracer provider, in a
// setupFilesAfterEnv file (the test sandbox). SDK 2.x providers take span
// processors only in their constructor, so the suite adds this one there;
// the collector never replaces, reconfigures, flushes or shuts down the
// suite's provider. Outside the collector's environment, or with OTel mode
// off, it is a processor that does nothing.
const NOOP_PROCESSOR = { onStart() {}, onEnd() {}, forceFlush: async () => {}, shutdown: async () => {} }

const sandboxApi = () => {
  try {
    return require('@opentelemetry/api')
  } catch {
    return null
  }
}

exports.createBuildkiteSpanProcessor = () => {
  const children = globalThis.__buildkiteOtel
  return children ? children.createSpanProcessor(sandboxApi()) : NOOP_PROCESSOR
}
