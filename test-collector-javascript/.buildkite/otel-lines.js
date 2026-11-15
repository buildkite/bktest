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
  const semver = require('semver')
  const requested = Object.entries(dependencies(process.argv[2]))
  execFileSync('npm', ['install', '--no-save', '--strict-peer-deps', ...requested.map(([name, version]) => `${name}@${version}`)], { stdio: 'inherit' })
  const { packageVersion } = require('../util/otel/sdk')
  // Fail if the collector would resolve a copy other than the one requested.
  let mismatched = false
  for (const [name, version] of requested) {
    const installed = packageVersion(name)
    console.log(name, installed)
    if (version !== 'latest' && !semver.satisfies(installed || '0.0.0', version)) mismatched = true
  }
  if (mismatched) process.exit(1)
}
module.exports = { dependencies }
