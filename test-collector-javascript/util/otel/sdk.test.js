const { loadSdk, createProvider } = require('./sdk')

const originalEnv = process.env
beforeEach(() => {
  process.env = Object.fromEntries(Object.entries(originalEnv).filter(([name]) => !name.startsWith('OTEL_')))
})
afterEach(() => { process.env = originalEnv })

// These decisions and limits were checked against sdk-trace-base 2.8.0's
// BasicTracerProvider. Keep its environment contract on both sdk-trace lines.
test.each([
  ['always_on', undefined, [true, true, true, true]],
  ['always_off', undefined, [false, false, false, false]],
  ['traceidratio', '0.25', [true, false, false, false]],
  ['parentbased_always_on', undefined, [true, true, false, true]],
  ['parentbased_always_off', undefined, [false, false, false, true]],
  ['parentbased_traceidratio', '0.25', [true, false, false, true]],
  ['parentbased_traceidratio', '0', [false, false, false, true]],
  ['parentbased_traceidratio', '1', [true, true, false, true]],
  ['parentbased_traceidratio', undefined, [true, true, false, true]],
  ['parentbased_traceidratio', '-0.1', [true, true, false, true]],
  ['parentbased_traceidratio', '1.1', [true, true, false, true]],
  ['parentbased_traceidratio', 'invalid', [true, true, false, true]],
  ['invalid', undefined, [true, true, false, true]],
  [undefined, undefined, [true, true, false, true]],
])('matches SDK 2 sampling for %s / %s', async (name, arg, expected) => {
  if (name !== undefined) process.env.OTEL_TRACES_SAMPLER = name
  if (arg !== undefined) process.env.OTEL_TRACES_SAMPLER_ARG = arg
  const sdk = loadSdk({ exporter: false })
  const low = '00000000000000000000000000000001', high = '000000000000000000000000eeeeeeee'
  const recording = []
  for (const traceId of [low, high]) {
    const provider = createProvider(sdk, { getIds: () => ({ traceId }) })
    const tracer = provider.getTracer('parity')
    if (traceId === low) {
      const span = tracer.startSpan('root', {}, sdk.api.ROOT_CONTEXT)
      recording.push(span.isRecording()); span.end()
    } else {
      for (const flags of [undefined, 0, 1]) {
        const context = flags === undefined ? sdk.api.ROOT_CONTEXT : sdk.api.trace.setSpanContext(sdk.api.ROOT_CONTEXT, {
          traceId: high, spanId: '0123456789abcdef', traceFlags: flags, isRemote: true,
        })
        const span = tracer.startSpan('child', {}, context)
        recording.push(span.isRecording()); span.end()
      }
      // ParentBased's local-parent decisions must match the remote ones too.
      for (const flags of [0, 1]) {
        const context = sdk.api.trace.setSpanContext(sdk.api.ROOT_CONTEXT, {
          traceId: high, spanId: '0123456789abcdef', traceFlags: flags, isRemote: false,
        })
        const span = tracer.startSpan('local child', {}, context)
        expect(span.isRecording()).toBe(expected[flags + 2]); span.end()
      }
    }
    await provider.shutdown()
  }
  expect(recording).toEqual(expected)
})

test.each([
  [undefined, undefined, { first: 'abc', second: 'uvw' }],
  ['1', '2', { first: 'ab' }],
  ['0', '0', {}],
  ['invalid', 'invalid', { first: 'abc', second: 'uvw' }],
])('matches SDK 2 span/general limit precedence (%s, %s)', async (count, length, attributes) => {
  Object.assign(process.env, {
    OTEL_ATTRIBUTE_COUNT_LIMIT: '2', OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT: '3',
    OTEL_SPAN_EVENT_COUNT_LIMIT: '1', OTEL_SPAN_ATTRIBUTE_PER_EVENT_COUNT_LIMIT: '1',
    OTEL_SPAN_LINK_COUNT_LIMIT: '1', OTEL_SPAN_ATTRIBUTE_PER_LINK_COUNT_LIMIT: '1',
  })
  if (count !== undefined) process.env.OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT = count
  if (length !== undefined) process.env.OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT = length
  const sdk = loadSdk({ exporter: false })
  const provider = createProvider(sdk)
  try {
    const values = { first: 'abcdef', second: 'uvwxyz', third: 'dropped' }
    const context = { traceId: '0123456789abcdef0123456789abcdef', spanId: '0123456789abcdef', traceFlags: 1 }
    const span = provider.getTracer('limits').startSpan('child', {
      attributes: values, links: [{ context }, { context, attributes: values }],
    })
    span.addEvent('discarded', values); span.addEvent('retained', values); span.end()
    expect(span.attributes).toEqual(attributes)
    expect(span.events.map(event => event.name)).toEqual(['retained'])
    expect(Object.keys(span.events[0].attributes)).toEqual(['first'])
    expect(span.links).toHaveLength(1)
    expect(Object.keys(span.links[0].attributes)).toEqual(['first'])
  } finally { await provider.shutdown() }
})
