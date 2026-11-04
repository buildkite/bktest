const fs = require('fs')
const os = require('os')
const path = require('path')
const { execFileSync } = require('child_process')

test.each([
  ['defaults', '', '', ['http']],
  ['explicit opt-in', ' fs , host-metrics , http ', '', ['fs', 'host-metrics', 'http']],
  ['partial opt-in', 'fs,http', '', ['fs', 'http']],
  ['disabled wins', 'fs,host-metrics,http', ' fs , host-metrics ', ['http']],
  ['disabled only', '', 'http', []],
])('instrumentation discovery follows auto-instrumentations-node %s', (_, enabled, disabled, expected) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'otel-selection-'))
  try {
    // Exercise declared dependencies and packages found only by scanning
    // node_modules, including non-hoisted auto-instrumentations dependencies.
    for (const nested of [false, true]) {
      const project = path.join(root, String(nested))
      const base = nested ? path.join(project, 'node_modules/@opentelemetry/auto-instrumentations-node') : project
      fs.mkdirSync(base, { recursive: true })
      fs.writeFileSync(path.join(project, 'package.json'), '{}')
      fs.writeFileSync(path.join(base, 'package.json'), JSON.stringify({
        main: 'index.js', dependencies: { '@opentelemetry/instrumentation-fs': '*' },
      }))
      fs.writeFileSync(path.join(base, 'index.js'), '')
      for (const name of ['fs', 'host-metrics', 'http']) {
        const directory = path.join(base, 'node_modules/@opentelemetry', `instrumentation-${name}`)
        fs.mkdirSync(directory, { recursive: true })
        fs.writeFileSync(path.join(directory, 'index.js'), `exports.TestInstrumentation = class { constructor() { this.instrumentationVersion = '0.223.0' } }`)
      }
      const sdk = path.join(base, 'node_modules/@opentelemetry/instrumentation')
      fs.mkdirSync(sdk)
      fs.writeFileSync(path.join(sdk, 'package.json'), JSON.stringify({ version: '0.223.0' }))
      const output = execFileSync(process.execPath, ['-e', `
        const { loadInstrumentations } = require(${JSON.stringify(require.resolve('./otelInstrumentations'))})
        console.log(JSON.stringify(loadInstrumentations(process.argv[1], (_, message) => { throw new Error(message) }).map(entry => entry.name).sort()))
      `, project], { env: { ...process.env, OTEL_NODE_ENABLED_INSTRUMENTATIONS: enabled, OTEL_NODE_DISABLED_INSTRUMENTATIONS: disabled } })
      expect(JSON.parse(output)).toEqual(expected)
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})
