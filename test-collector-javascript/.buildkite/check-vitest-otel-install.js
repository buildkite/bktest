const fs = require('fs')
const os = require('os')
const path = require('path')
const assert = require('assert/strict')
const { execFile, execFileSync } = require('child_process')
const { promisify } = require('util')
const { startOtlpServer } = require('../e2e/support/otlpServer')
const { dependencies } = require('./otel-lines')

// Test the published layout, with only the auto-instrumentations bundle
// declaring http/undici. Workspace hoisting must not make this pass by accident.
const main = async () => {
  const manager = process.argv[2] || 'npm'
  const line = process.argv[3] || '2'
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'vitest-otel-install-'))
  let server
  try {
    const packed = JSON.parse(execFileSync('npm', ['pack', '--json', '--pack-destination', scratch], { encoding: 'utf8' }))[0]
    fs.writeFileSync(path.join(scratch, 'package.json'), JSON.stringify({ private: true, type: 'module', devDependencies: {
      'buildkite-test-collector': `file:${path.join(scratch, packed.filename)}`,
      vitest: '5.1.0', vite: '^8',
      ...Object.fromEntries(Object.entries(dependencies(line)).filter(([name]) => !name.includes('/instrumentation-'))),
      '@opentelemetry/auto-instrumentations-node': line === '2' ? '0.81.0' : '0.83.0',
    } }))
    execFileSync(manager, ['install', '--ignore-scripts', manager === 'pnpm' ? '--strict-peer-dependencies' : '--strict-peer-deps'], { cwd: scratch, stdio: 'inherit' })
    fs.cpSync(path.join(__dirname, '../examples/vitest/otel'), path.join(scratch, 'otel'), { recursive: true })
    fs.writeFileSync(path.join(scratch, 'vitest.config.js'), `
      import { defineConfig } from 'vitest/config'
      import { createRequire } from 'node:module'
      const require = createRequire(import.meta.url)
      export default defineConfig({ test: {
        include: ['otel/core.js', 'otel/second.js'], maxWorkers: 1,
        reporters: ['default', ['buildkite-test-collector/vitest/reporter', { otelEnabled: true }]],
        experimental: { openTelemetry: { enabled: true, sdkPath: require.resolve('buildkite-test-collector/vitest/otel.js') } },
      } })
    `)
    server = await startOtlpServer()
    const result = await promisify(execFile)(process.execPath, ['node_modules/vitest/vitest.mjs', 'run'], {
      cwd: scratch, env: { ...process.env, BUILDKITE_ANALYTICS_TOKEN: 'install-test',
        BUILDKITE_ANALYTICS_KEY: 'install-test', BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url },
    }).catch(error => error)
    assert.equal(result.code, 1, result.stderr) // Intentional hook failure.
    assert(!result.stderr.includes('TEST RESULTS MISSING'), result.stderr)
    const spans = server.spans()
    assert.equal(spans.filter(s => s.name === 'test.execution').length, 8, result.stderr)
    assert.equal(spans.filter(s => s.kind === 3).length, 2, result.stderr)
    assert.equal(spans.filter(s => s.kind === 2).length, 2, result.stderr)
    console.log(`${manager}: packed install passed; 8 executions, HTTP and fetch from auto-instrumentations only`)
  } finally {
    await server?.close()
    fs.rmSync(scratch, { recursive: true, force: true })
  }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
