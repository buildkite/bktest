const fs = require('fs')
const os = require('os')
const path = require('path')
const { execFileSync } = require('child_process')

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
        fs.writeFileSync(path.join(directory, 'index.js'), `exports.TestInstrumentation = class { constructor() { this.instrumentationVersion = '${name === 'undici' ? '0.33.0' : '0.223.0'}' } }`)
      }
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
