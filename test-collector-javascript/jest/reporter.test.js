const nock = require('nock')
const Reporter = require('./reporter')

test('omits the token from JSON reports through copying wrappers without changing caller options or watch-mode authentication', async () => {
  class CopyingReporter extends Reporter {
    constructor(config, options) { super(config, { ...options, tags: { suite: 'wrapper' } }) }
  }
  const token = 'jest-reporter-option-secret'
  const upload = nock('http://collector.test', {
    reqheaders: { authorization: `Token token="${token}"` },
  }).post('/uploads').times(4).reply(200)

  try {
    for (const hiddenToken of [false, true]) {
      const options = Object.freeze(Object.defineProperty({
        url: 'http://collector.test/uploads', tags: { suite: 'jest' }, password: 'private-password',
        toJSON() { return { token: this.token, url: this.url, tags: this.tags } },
      }, 'token', { value: token, enumerable: !hiddenToken }))
      const otherOptions = { title: 'HTML report' }
      const globalConfig = Object.freeze({
        rootDir: __dirname,
        reporters: [['html-reporter', otherOptions], [require.resolve('./reporter'), options]],
      })
      const ReporterClass = hiddenToken ? Reporter : CopyingReporter

      // Jest recreates reporters from this config for each watch-mode run.
      for (let run = 0; run < 2; run++) {
        const reporter = new ReporterClass(globalConfig, globalConfig.reporters[1][1])
        expect(JSON.stringify(globalConfig)).not.toContain(token)
        expect(JSON.stringify(globalConfig)).not.toContain('private-password')
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
      expect(globalConfig.reporters[0][1]).toBe(otherOptions)
      expect(options.token).toBe(token)
      expect(JSON.stringify(options)).toContain(token)
      expect(JSON.stringify(options)).not.toContain('private-password')
    }
    expect(upload.isDone()).toBe(true)
  } finally {
    nock.cleanAll()
  }
})
