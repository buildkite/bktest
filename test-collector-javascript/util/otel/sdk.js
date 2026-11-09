const { createRequire } = require('module')
const fs = require('fs')
const path = require('path')
const semver = require('semver')
const { randomBytes } = require('crypto')
const { resourceAttributes } = require('./attributes')
const { describeError, warnOncePerRun } = require('./warn')

const PACKAGES = {
  trace: '@opentelemetry/sdk-trace',
  context: '@opentelemetry/context-async-hooks',
  exporter: '@opentelemetry/exporter-trace-otlp-proto',
}

// Floors, not caps. sdk-trace exists from 2.9.0 and exporter 0.220.0 is the
// first built on it. CI installs explicit, matching SDK lines.
const SUPPORTED_VERSIONS = {
  '@opentelemetry/sdk-trace': '>=2.9.0',
  '@opentelemetry/context-async-hooks': '>=2.0.0',
  '@opentelemetry/exporter-trace-otlp-proto': '>=0.220.0',
}
// SDK 3 packages (and 0.300+ instrumentation) need api 1.10 and Node 22.15.
const SDK3_NODE = '>=22.15.0'

const installCommand = () => {
  let apiVersion
  try { apiVersion = packageVersion('@opentelemetry/api') } catch { /* API not installed yet. */ }
  const sdk3 = semver.satisfies(process.version, SDK3_NODE) && apiVersion && semver.gte(apiVersion, '1.10.0')
  return sdk3
    ? 'npm install --save-dev "@opentelemetry/api@^1.10" "@opentelemetry/sdk-trace@^3" "@opentelemetry/context-async-hooks@^3" "@opentelemetry/exporter-trace-otlp-proto@~0.301.0"'
    : 'npm install --save-dev "@opentelemetry/api@~1.9.0" "@opentelemetry/sdk-trace@^2.9" "@opentelemetry/context-async-hooks@^2" "@opentelemetry/exporter-trace-otlp-proto@~0.223.0"'
}

const packageVersion = (name, load = require) => {
  for (let directory = path.dirname(load.resolve(name)); directory !== path.dirname(directory); directory = path.dirname(directory)) {
    const file = path.join(directory, 'package.json')
    if (fs.existsSync(file) && require(file).name === name) return require(file).version
  }
}

// Optional packages must not load until OpenTelemetry is requested.
const loadSdk = ({ exporter = true, context = false } = {}) => {
  const sdk = {}
  try {
    for (const [key, name] of Object.entries(PACKAGES)) {
      if (key === 'context' && !context) continue
      const range = SUPPORTED_VERSIONS[name]
      const version = packageVersion(name)
      if (version && !semver.satisfies(version, range)) throw new Error(`unsupported ${name}@${version} (requires ${range})`)
      if (version && semver.major(version) >= 3 && !semver.satisfies(process.version, SDK3_NODE)) {
        warnOncePerRun(undefined, 'sdk3-node', `OpenTelemetry SDK 3 requires Node.js ${SDK3_NODE}; running on ${process.version} is unsupported. Export will still be attempted; upgrade Node.js if it fails.`)
      }
      if (key !== 'exporter' || exporter) sdk[key] = require(name)
    }
    const fromTrace = createRequire(require.resolve(PACKAGES.trace))
    for (const key of ['api', 'resources', 'core']) sdk[key] = fromTrace(`@opentelemetry/${key}`)
    sdk.apiVersion = packageVersion('@opentelemetry/api', fromTrace)
  } catch (error) {
    // The first line names the module; the rest is the require stack.
    throw new Error(`${String(error.message).split('\n')[0]}. Install the OpenTelemetry packages with: ${installCommand()}`)
  }
  return sdk
}

const SPAN_LIMITS = {
  attributeCountLimit: 128, attributeValueLengthLimit: 2 * 16384,
  eventCountLimit: 100, attributePerEventCountLimit: 128,
  linkCountLimit: 128, attributePerLinkCountLimit: 128,
}

// Suites can stub Math.random. Never let that produce invalid all-zero IDs.
const randomHex = (bytes) => {
  for (;;) {
    const hex = randomBytes(bytes).toString('hex')
    if (/[^0]/.test(hex)) return hex
  }
}

const createResource = ({ resources }, runEnv) => resources.defaultResource()
  .merge(resources.detectResources({ detectors: [resources.envDetector] }))
  .merge(resources.resourceFromAttributes(resourceAttributes(runEnv)))

// sdk-trace reads no environment on either line. Children follow the
// OTEL_TRACES_SAMPLER and span limit variables, as sdk-trace-base's
// BasicTracerProvider (loadDefaultConfig/reconfigureLimits) did.
const SAMPLERS = {
  always_on: (trace) => new trace.AlwaysOnSampler(),
  always_off: (trace) => new trace.AlwaysOffSampler(),
  traceidratio: (trace, ratio) => new trace.TraceIdRatioBasedSampler(ratio),
  parentbased_always_on: (trace) => new trace.ParentBasedSampler({ root: new trace.AlwaysOnSampler() }),
  parentbased_always_off: (trace) => new trace.ParentBasedSampler({ root: new trace.AlwaysOffSampler() }),
  parentbased_traceidratio: (trace, ratio) => new trace.ParentBasedSampler({ root: new trace.TraceIdRatioBasedSampler(ratio) }),
}
const environmentConfig = ({ trace, core, api }) => {
  const number = (name) => core.getNumberFromEnv(name)
  let name = core.getStringFromEnv('OTEL_TRACES_SAMPLER') ?? 'parentbased_always_on'
  if (!SAMPLERS[name]) {
    api.diag.error(`OTEL_TRACES_SAMPLER value "${name}" invalid, defaulting to "parentbased_always_on".`)
    name = 'parentbased_always_on'
  }
  let ratio = number('OTEL_TRACES_SAMPLER_ARG')
  if (name.endsWith('traceidratio') && !(ratio >= 0 && ratio <= 1)) {
    api.diag.error(`OTEL_TRACES_SAMPLER_ARG=${ratio} is blank or out of range [0..1], defaulting to 1.`)
    ratio = 1
  }
  return {
    sampler: SAMPLERS[name](trace, ratio),
    spanLimits: {
      attributeValueLengthLimit: number('OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT') ?? number('OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT') ?? Infinity,
      attributeCountLimit: number('OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT') ?? number('OTEL_ATTRIBUTE_COUNT_LIMIT') ?? 128,
      linkCountLimit: number('OTEL_SPAN_LINK_COUNT_LIMIT') ?? 128,
      eventCountLimit: number('OTEL_SPAN_EVENT_COUNT_LIMIT') ?? 128,
      attributePerEventCountLimit: number('OTEL_SPAN_ATTRIBUTE_PER_EVENT_COUNT_LIMIT') ?? 128,
      attributePerLinkCountLimit: number('OTEL_SPAN_ATTRIBUTE_PER_LINK_COUNT_LIMIT') ?? 128,
    },
  }
}

// Only test.execution ignores environment-selected sampling and span limits.
const createProvider = (sdk, { testSpans = false, getIds = () => null, ...config } = {}) => new sdk.trace.TracerProvider({
  idGenerator: {
    generateTraceId: () => getIds()?.traceId || randomHex(16),
    generateSpanId: () => getIds()?.spanId || randomHex(8),
  },
  ...(testSpans ? { sampler: new sdk.trace.AlwaysOnSampler(), spanLimits: SPAN_LIMITS } : environmentConfig(sdk)),
  ...config,
})

const createContextManager = (sdk = loadSdk({ exporter: false, context: true })) => new sdk.context.AsyncLocalStorageContextManager().enable()

const createPropagator = (sdk) => new sdk.core.CompositePropagator({
  propagators: [new sdk.core.W3CTraceContextPropagator(), new sdk.core.W3CBaggagePropagator()],
})

// Mixed SDK lines can create spans but fail serialization; probe before tests run.
const assertCanExport = (sdk, providerConfig) => {
  try {
    const exporterRequire = createRequire(require.resolve(PACKAGES.exporter))
    const { ProtobufTraceSerializer } = exporterRequire('@opentelemetry/otlp-transformer')
    const span = createProvider(sdk, providerConfig).getTracer('buildkite-test-collector').startSpan('probe')
    span.end()
    ProtobufTraceSerializer.serializeRequest([span])
  } catch (error) {
    throw new Error(
      `the installed OpenTelemetry packages cannot export spans together (${describeError(error)}). ` +
      `Install matching versions with: ${installCommand()}`
    )
  }
}

module.exports = { loadSdk, createProvider, createResource, createContextManager, createPropagator, assertCanExport, SUPPORTED_VERSIONS, packageVersion }
