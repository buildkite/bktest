const fs = require('fs')
const os = require('os')
const path = require('path')
const { execFileSync } = require('child_process')

// Exercise npm's peer resolver against the shipped package, not a workspace link.
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'collector-installs-'))
try {
  const packed = JSON.parse(execFileSync('npm', ['pack', '--json', '--pack-destination', scratch], { encoding: 'utf8' }))[0]
  for (const [name, version] of [
    ['@opentelemetry/sdk-node', '0.223.0'],
    ['@opentelemetry/auto-instrumentations-node', '0.81.0'],
  ]) {
    const cwd = path.join(scratch, name.split('/')[1])
    fs.mkdirSync(cwd)
    fs.writeFileSync(path.join(cwd, 'package.json'), JSON.stringify({ private: true, dependencies: {
      [name]: version,
      'buildkite-test-collector': `file:${path.join(scratch, packed.filename)}`,
    } }))
    for (const args of [['install'], ['ci', '--strict-peer-deps']]) {
      console.log(`${name}@${version}: npm ${args.join(' ')}`)
      execFileSync('npm', [...args, '--ignore-scripts', '--no-audit', '--no-fund'], { cwd, stdio: 'inherit' })
    }
  }
} finally {
  fs.rmSync(scratch, { recursive: true, force: true })
}
