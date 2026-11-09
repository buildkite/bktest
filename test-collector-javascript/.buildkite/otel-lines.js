// Install a coherent line, never the collector's uncapped support ranges:
// npm cannot pair SDK 2's api <1.10 peer with SDK 3's api >=1.10 peer.
const lines = {
  '2': {
    api: '~1.9.0', 'sdk-trace': '^2.9', resources: '^2.9', 'context-async-hooks': '^2.9',
    'exporter-trace-otlp-proto': '^0.223.0', 'instrumentation-http': '^0.223.0',
    'instrumentation-undici': '^0.33.0', 'instrumentation-pg': '^0.69.0',
    'instrumentation-express': '^0.71.1', 'instrumentation-graphql': '^0.69.0', 'instrumentation-redis': '^0.71.0',
  },
  '3': {
    api: '^1.10', 'sdk-trace': '^3', resources: '^3', 'context-async-hooks': '^3',
    'exporter-trace-otlp-proto': '^0.301.0', 'instrumentation-http': '^0.301.0',
    'instrumentation-undici': '^0.35.0', 'instrumentation-pg': '^0.78.0',
    'instrumentation-express': '^0.73.0', 'instrumentation-graphql': '^0.73.0', 'instrumentation-redis': '^0.73.0',
  },
}
lines.floor = { ...lines['2'], api: '1.9.0', 'sdk-trace': '2.9.0', resources: '2.9.0',
  'context-async-hooks': '2.9.0', 'exporter-trace-otlp-proto': '0.220.0',
  'instrumentation-http': '0.220.0', 'instrumentation-undici': '0.30.0' }
lines.latest = Object.fromEntries(Object.keys(lines['3']).map(name => [name, 'latest']))
const dependencies = line => Object.fromEntries(Object.entries(lines[line]).map(([name, version]) => [`@opentelemetry/${name}`, version]))

if (require.main === module) {
  const { execFileSync } = require('child_process')
  const specs = Object.entries(dependencies(process.argv[2])).map(([name, version]) => `${name}@${version}`)
  execFileSync('npm', ['install', '--no-save', '--strict-peer-deps', ...specs], { stdio: 'inherit' })
  const { packageVersion } = require('../util/otel/sdk')
  for (const name of Object.keys(dependencies(process.argv[2]))) console.log(name, packageVersion(name))
}
module.exports = { dependencies }
