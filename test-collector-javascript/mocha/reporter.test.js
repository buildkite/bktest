const { execFile } = require('child_process')
const fs = require('fs')
const http = require('http')
const os = require('os')
const path = require('path')

describe.each(['mocha', 'cypress'])('%s reporter', (reporter) => {
  test('uploads passed, failed and pending results through mocha-multi-reporters', async () => {
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'collector-mocha-'))
    const uploads = []
    const server = http.createServer((request, response) => {
      let body = ''
      request.on('data', (chunk) => { body += chunk })
      request.on('end', () => {
        uploads.push(JSON.parse(body))
        response.end('{}')
      })
    })

    try {
      fs.writeFileSync(path.join(directory, 'test.cjs'), `
        const assert = require('node:assert/strict')
        describe('outcomes', () => {
          it('passes', () => assert.equal(1 + 2, 3))
          it('fails', () => assert.equal(1 + 2, 4))
          it.skip('pending', () => {})
        })
      `)
      fs.writeFileSync(path.join(directory, 'reporters.json'), JSON.stringify({
        reporterEnabled: `spec, ${require.resolve(`../${reporter}/reporter`)}`,
      }))
      await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))

      const { error, stderr } = await new Promise((resolve) => {
        execFile(process.execPath, [
          require.resolve('mocha/bin/mocha.js'), path.join(directory, 'test.cjs'),
          '--reporter', require.resolve('mocha-multi-reporters'),
          '--reporter-options', `configFile=${path.join(directory, 'reporters.json')}`,
          '--exit',
        ], {
          env: {
            ...process.env,
            BUILDKITE_ANALYTICS_TOKEN: 'dummy-token',
            BUILDKITE_ANALYTICS_DEBUG_ENABLED: 'false',
            BUILDKITE_ANALYTICS_BASE_URL: `http://127.0.0.1:${server.address().port}/uploads`,
            NO_PROXY: '127.0.0.1',
          },
          timeout: 10000,
        }, (error, stdout, stderr) => resolve({ error, stdout, stderr }))
      })

      expect(stderr).toBe('')
      expect(error).toMatchObject({ code: 1 })
      expect(uploads).toHaveLength(1)
      expect(uploads[0].run_env.test_runner).toBe(reporter)
      expect(uploads[0].data.map(({ name, result }) => ({ name, result }))).toEqual([
        { name: 'passes', result: 'passed' },
        { name: 'fails', result: 'failed' },
        { name: 'pending', result: 'pending' },
      ])
    } finally {
      await new Promise((resolve) => server.close(resolve))
      fs.rmSync(directory, { recursive: true, force: true })
    }
  }, 15000)
})
