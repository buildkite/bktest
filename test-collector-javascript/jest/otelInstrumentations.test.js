const fs = require('fs')
const os = require('os')
const path = require('path')
const { execFileSync } = require('child_process')

let apiDirectory = path.dirname(require.resolve('@opentelemetry/api'))
while (!fs.existsSync(path.join(apiDirectory, 'package.json'))) apiDirectory = path.dirname(apiDirectory)

const excluded = ['fs', 'host-metrics', 'pino', 'winston', 'bunyan', 'aws-sdk', 'kafkajs', 'amqplib', 'net', 'dns', 'runtime-node',
  'fetch', 'xml-http-request', 'document-load', 'user-interaction', 'long-task']

test.each([
  ['defaults', '', '', ['http']],
  ['explicit opt-in', ' fs , host-metrics , http ', '', ['fs', 'host-metrics', 'http']],
  ['partial opt-in', 'fs,http', '', ['fs', 'http']],
  ['disabled wins', 'fs,host-metrics,http', ' fs , host-metrics ', ['http']],
  ['disabled only', '', 'http', []],
  ['all exclusions opt-in', excluded.join(','), '', [...excluded].sort()],
  ...['pino', 'winston', 'bunyan'].flatMap((name) => [
    [`${name} opt-in alone`, name, '', [name]],
    [`${name} disabled wins`, name, name, []],
  ]),
])('declared instrumentation discovery respects %s', (_, enabled, disabled, expected) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-selection-'))
  try {
    // The same declared set is selected from a project, its workspace
    // ancestor, or a non-hoisted auto-instrumentations bundle.
    for (const layout of ['project', 'ancestor', 'bundle']) {
      const workspace = path.join(root, layout)
      const project = path.join(workspace, 'packages/tests')
      const base = layout === 'bundle' ? path.join(project, 'node_modules/@opentelemetry/auto-instrumentations-node')
        : layout === 'ancestor' ? workspace : project
      fs.mkdirSync(project, { recursive: true })
      fs.mkdirSync(base, { recursive: true })
      fs.writeFileSync(path.join(base, 'package.json'), JSON.stringify({
        main: 'index.js', dependencies: Object.fromEntries([...excluded, 'http'].map((name) => [`@opentelemetry/instrumentation-${name}`, '*'])),
      }))
      fs.writeFileSync(path.join(base, 'index.js'), '')
      // Undici is visible but undeclared, as when hoisted from @sentry/node.
      for (const name of [...excluded, 'http', 'undici']) {
        const directory = path.join(base, 'node_modules/@opentelemetry', `instrumentation-${name}`)
        fs.mkdirSync(directory, { recursive: true })
        fs.writeFileSync(path.join(directory, 'index.js'),
          `exports.TestInstrumentation = class { constructor() { this.instrumentationVersion = '${name === 'undici' ? '0.33.0' : '0.223.0'}' } setMeterProvider() {} }`)
      }
      fs.symlinkSync(apiDirectory, path.join(base, 'node_modules/@opentelemetry/api'), 'dir')
      const sdk = path.join(base, 'node_modules/@opentelemetry/instrumentation')
      fs.mkdirSync(sdk)
      fs.writeFileSync(path.join(sdk, 'package.json'), JSON.stringify({ name: '@opentelemetry/instrumentation', version: '0.223.0', exports: './index.js' }))
      fs.writeFileSync(path.join(sdk, 'index.js'), '')
      const output = execFileSync(process.execPath, ['-e', `
        const { loadInstrumentations } = require(${JSON.stringify(require.resolve('../util/otel/instrumentations'))})
        console.log(JSON.stringify(loadInstrumentations(process.argv[1], (_, message) => { throw new Error(message) }).map(entry => entry.name).sort()))
      `, project], { env: { ...process.env, OTEL_NODE_ENABLED_INSTRUMENTATIONS: enabled, OTEL_NODE_DISABLED_INSTRUMENTATIONS: disabled } })
      expect(JSON.parse(output)).toEqual(expected)
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test.each([
  ['http', 'active', 1],
  ['http', 'disabled', 0],
  ['http', 'loaded', 0],
  ['http', 'absent', 0],
  ['undici', 'active', 0],
  ['pg', 'active', 0],
])('dd-trace coexistence warning preserves %s selection (DD HTTP %s)', (enabled, state, count) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-dd-trace-'))
  try {
    fs.symlinkSync(path.join(__dirname, '../node_modules'), path.join(root, 'node_modules'), 'dir')
    fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify({ dependencies: {
      '@opentelemetry/instrumentation-http': '*',
      '@opentelemetry/instrumentation-undici': '*',
      '@opentelemetry/instrumentation-pg': '*',
    } }))
    const output = execFileSync(process.execPath, ['-e', `
      const { loadInstrumentations } = require(${JSON.stringify(require.resolve('../util/otel/instrumentations'))})
      const { warnOncePerRun } = require(${JSON.stringify(require.resolve('../util/otel/warn'))})
      // Only dd-trace 5's already-active HTTP client is detected at startup.
      if (${JSON.stringify(state)} !== 'absent') globalThis._ddtrace = {
        _tracingInitialized: ${state !== 'loaded'},
        _pluginManager: { _pluginsByName: { http: { client: { _enabled: ${state !== 'disabled'} } } } },
      }
      const warnings = []
      console.warn = message => warnings.push(message)
      const selected = []
      for (const directory of [process.argv[1], process.argv[1] + '/second-project']) {
        selected.push(loadInstrumentations(directory, (key, message) => warnOncePerRun(undefined, key, message)).map(entry => entry.name))
      }
      console.log(JSON.stringify({ warnings, selected, loaded: Boolean(globalThis._ddtrace) }))
    `, root], { env: { ...process.env, OTEL_NODE_ENABLED_INSTRUMENTATIONS: enabled, OTEL_NODE_DISABLED_INSTRUMENTATIONS: '' } })
    const result = JSON.parse(output)
    expect(result.selected).toEqual([[enabled], [enabled]])
    expect(result.loaded).toBe(state !== 'absent')
    expect(result.warnings).toHaveLength(count)
    if (count) {
      expect(result.warnings[0]).toMatch(/dd-trace.*traceparent.*HTTP server child spans may be missing/)
      expect(result.warnings[0]).toContain('Configure only one tracer to instrument HTTP/fetch/undici')
      expect(result.warnings[0]).toContain('test.execution export remains enabled')
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test.each(['shared loader', 'Jest sandbox'])('records metrics after subclass field initialization in %s', (mode) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-metrics-'))
  try {
    const instrumentation = path.join(root, 'node_modules/@opentelemetry/instrumentation-http')
    const client = path.join(root, 'node_modules/metric-client/index.js')
    fs.mkdirSync(instrumentation, { recursive: true })
    fs.mkdirSync(path.dirname(client), { recursive: true })
    fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify({ dependencies: { '@opentelemetry/instrumentation-http': '*' } }))
    fs.writeFileSync(path.join(path.dirname(client), 'package.json'), JSON.stringify({ version: '1.0.0' }))
    fs.symlinkSync(apiDirectory, path.join(root, 'node_modules/@opentelemetry/api'), 'dir')
    fs.writeFileSync(path.join(instrumentation, 'index.js'), `
      const { InstrumentationBase } = require(${JSON.stringify(require.resolve('@opentelemetry/instrumentation'))})
      exports.TestInstrumentation = class extends InstrumentationBase {
        duration // Like OpenAI, this overwrites the instrument created by super().
        constructor(config) { super('fixture', '0.223.0', config) }
        _updateMetricInstruments() { this.duration = this.meter.createHistogram('duration') }
        record() { this.duration.record(1) }
        init() { return [{ name: 'metric-client', supportedVersions: ['*'], patch: () => () => this.record() }] }
      }
    `)
    execFileSync(process.execPath, ['-e', `
      const assert = require('node:assert/strict')
      const api = require(${JSON.stringify(require.resolve('@opentelemetry/api'))})
      const { loadInstrumentations } = require(${JSON.stringify(require.resolve('../util/otel/instrumentations'))})
      const entries = loadInstrumentations(${JSON.stringify(root)}, (_, message) => { throw new Error(message) })
      assert.equal(entries.length, 1)
      let record = () => entries[0].instance.record()
      if (${JSON.stringify(mode)} === 'Jest sandbox') {
        const Packages = require(${JSON.stringify(require.resolve('./otelPackages'))})
        const packages = new Packages({
          testPath: ${JSON.stringify(path.join(root, 'test.js'))}, cleanups: [],
          warnOnce: (_, message) => { throw new Error(message) },
          _worker: () => ({ tracerProvider: api.trace.getTracerProvider() }),
          runtime: {
            _resolveCjsModule: () => ${JSON.stringify(client)},
            requireModuleOrMock: () => api,
            requireModule: () => require(${JSON.stringify(instrumentation)}),
          },
        }, entries)
        record = packages.load(${JSON.stringify(client)}, () => () => { throw new Error('client was not patched') })
      }
      assert.doesNotThrow(record)
    `], { env: { ...process.env, OTEL_NODE_ENABLED_INSTRUMENTATIONS: 'http', OTEL_NODE_DISABLED_INSTRUMENTATIONS: '' } })
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})
