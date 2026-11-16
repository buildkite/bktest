const fs = require('fs')
const os = require('os')
const path = require('path')
const { execFileSync } = require('child_process')
const { dependencies } = require('./otel-lines')

// Exercise npm's peer resolver against the shipped package, not a workspace link.
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'collector-installs-'))
try {
  const packed = JSON.parse(execFileSync('npm', ['pack', '--json', '--pack-destination', scratch], { encoding: 'utf8' }))[0]
  for (const line of ['2', '3']) for (const [name, version] of [
    ['@opentelemetry/sdk-node', line === '2' ? '^0.223.0' : '^0.301.0'],
    ['@opentelemetry/auto-instrumentations-node', line === '2' ? '0.81.0' : '0.83.0'],
  ]) {
    const cwd = path.join(scratch, `${line}-${name.split('/')[1]}`)
    fs.mkdirSync(cwd)
    fs.writeFileSync(path.join(cwd, 'package.json'), JSON.stringify({ private: true, dependencies: {
      ...dependencies(line),
      [name]: version,
      'buildkite-test-collector': `file:${path.join(scratch, packed.filename)}`,
    } }))
    for (const args of [['install', '--strict-peer-deps'], ['ci', '--strict-peer-deps']]) {
      console.log(`${name}@${version}: npm ${args.join(' ')}`)
      execFileSync('npm', [...args, '--ignore-scripts', '--no-audit', '--no-fund'], { cwd, stdio: 'inherit' })
    }
  }
} finally {
  fs.rmSync(scratch, { recursive: true, force: true })
}
