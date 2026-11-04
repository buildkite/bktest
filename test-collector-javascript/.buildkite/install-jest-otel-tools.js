const path = require('path')
const { createRequire } = require('module')
const { execFileSync } = require('child_process')

const version = require(require.resolve('jest/package.json', { paths: [path.join(__dirname, '../examples/jest')] })).version
const directory = path.join(__dirname, '../node_modules/.cache/jest-otel-tools', version)
const load = createRequire(path.join(directory, 'package.json'))
if (require.main === module && Number(version.split('.')[0]) >= 29) {
  execFileSync('npm', ['install', '--prefix', directory, '--no-save', '--package-lock=false', '--ignore-scripts',
    `jest-environment-jsdom@${version}`, '@happy-dom/jest-environment@20.8.4', 'typescript@5.9.3'], { stdio: 'inherit' })
}
module.exports = load
