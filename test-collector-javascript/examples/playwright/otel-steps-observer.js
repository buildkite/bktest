const fs = require('fs')

// Independent oracle: preserve Playwright's complete step tree, not just the
// test.step subset included by its JSON reporter.
const describeStep = step => ({
  title: step.title,
  category: step.category,
  start: step.startTime.getTime(),
  duration: step.duration,
  error: Boolean(step.error),
  steps: step.steps.map(describeStep),
})

module.exports = class {
  constructor() { this.attempts = [] }
  onBegin() {
    if (process.env.PLAYWRIGHT_OTEL_OTHER_SIGTERM) {
      process.once('SIGTERM', () => {
        setTimeout(() => {
          console.log('other SIGTERM cleanup finished')
          process.exit(42)
        }, 6500)
      })
    }
  }
  onTestEnd(test, result) {
    this.attempts.push({ name: test.title, start: result.startTime.getTime(), steps: result.steps.map(describeStep) })
  }
  onEnd() {
    if (process.env.PLAYWRIGHT_OTEL_TERM_ON_END) {
      setTimeout(() => {
        fs.writeFileSync(process.env.PLAYWRIGHT_JSON_OUTPUT_NAME + '.signal', String(Date.now()))
        process.kill(process.pid, 'SIGTERM')
      }, 1000)
    }
    if (process.env.PLAYWRIGHT_JSON_OUTPUT_NAME) {
      fs.writeFileSync(process.env.PLAYWRIGHT_JSON_OUTPUT_NAME + '.steps', JSON.stringify(this.attempts))
    }
  }
}
