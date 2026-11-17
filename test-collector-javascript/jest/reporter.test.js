const nock = require('nock')
const Reporter = require('./reporter')

test('omits the token from JSON reports through copying wrappers without changing caller options or watch-mode authentication', async () => {
  class CopyingReporter extends Reporter {
    constructor(config, options) { super(config, { ...options, tags: { suite: 'wrapper' } }) }
  }
  const token = 'jest-reporter-option-secret'
  const options = Object.freeze({ token, url: 'http://collector.test/uploads', tags: { suite: 'jest' } })
  const otherOptions = { title: 'HTML report' }
  const globalConfig = Object.freeze({
    rootDir: __dirname,
    reporters: [['html-reporter', otherOptions], [require.resolve('./reporter'), options]],
  })
  const upload = nock('http://collector.test', {
    reqheaders: { authorization: `Token token="${token}"` },
  }).post('/uploads').twice().reply(200)

  try {
    // Jest recreates reporters from this config for each watch-mode run.
    for (let run = 0; run < 2; run++) {
      const reporter = new CopyingReporter(globalConfig, globalConfig.reporters[1][1])
      expect(JSON.stringify(globalConfig)).not.toContain(token)
      expect(JSON.parse(JSON.stringify(globalConfig)).reporters[1][1]).toEqual({
        url: options.url, tags: options.tags,
      })
      expect({ ...globalConfig.reporters[1][1] }.token).toBe(token)
      reporter.onTestResult({}, {
        testFilePath: __filename,
        perfStats: { start: 0, end: 100 },
        testResults: [{ title: 'passes', ancestorTitles: [], status: 'passed', duration: 100 }],
      })
      await reporter.onRunComplete()
    }
    expect(upload.isDone()).toBe(true)
    expect(globalConfig.reporters[0][1]).toBe(otherOptions)
    expect(options).toEqual({ token, url: options.url, tags: { suite: 'jest' } })
    expect(JSON.stringify(options)).toContain(token)
  } finally {
    nock.cleanAll()
  }
})
