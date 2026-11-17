const nock = require('nock')
const uploadTestResults = require('./uploadTestResults')

it('describes each debug upload without logging its authorization token', async () => {
  const previousDebug = process.env.BUILDKITE_ANALYTICS_DEBUG_ENABLED
  const previousToken = process.env.BUILDKITE_ANALYTICS_TOKEN
  process.env.BUILDKITE_ANALYTICS_DEBUG_ENABLED = 'true'
  const log = jest.spyOn(console, 'log').mockImplementation()
  const token = 'bkua_FAKE_upload_debug_regression'
  const url = 'https://collector.example.test/v1/uploads'
  const server = nock('https://collector.example.test', {
    reqheaders: { authorization: `Token token="${token}"` },
  })
    .post('/v1/uploads').reply(200, { ok: true })
    .post('/v1/uploads').reply(401, { error: 'Unauthorized' })
    .post('/v1/uploads').replyWithError('connection reset')

  try {
    for (let i = 0; i < 3; i++) {
      process.env.BUILDKITE_ANALYTICS_TOKEN = token
      await uploadTestResults({}, {}, ['debug result'], { token: i === 1 ? undefined : token, url })
    }

    expect(server.isDone()).toBe(true)
    const output = log.mock.calls.flat().join('\n')
    expect(output).not.toContain(token)
    const requests = log.mock.calls.flat().filter(line => line.includes('Test Engine Sending:'))
    expect(requests).toHaveLength(3)
    for (const request of requests) {
      expect(request).toContain('post')
      expect(request).toContain(url)
      expect(request).toContain('debug result')
    }
    expect(output).toContain('401')
    expect(output).toContain('connection reset')

    // Nock's unmatched-request error includes the request's Authorization header.
    nock('https://collector.example.test').post('/different-route').reply(200, {})
    for (const debug of ['false', 'true']) {
      log.mockClear()
      process.env.BUILDKITE_ANALYTICS_DEBUG_ENABLED = debug
      await uploadTestResults({}, {}, ['debug result'], { token, url })
      const errorOutput = log.mock.calls.flat().join('\n')
      expect(errorOutput).toContain('Nock: No match for request')
      expect(errorOutput).not.toContain(token)
      expect(errorOutput).toContain('[REDACTED]')
    }
  } finally {
    log.mockRestore()
    nock.cleanAll()
    if (previousDebug === undefined) delete process.env.BUILDKITE_ANALYTICS_DEBUG_ENABLED
    else process.env.BUILDKITE_ANALYTICS_DEBUG_ENABLED = previousDebug
    if (previousToken === undefined) delete process.env.BUILDKITE_ANALYTICS_TOKEN
    else process.env.BUILDKITE_ANALYTICS_TOKEN = previousToken
  }
})
