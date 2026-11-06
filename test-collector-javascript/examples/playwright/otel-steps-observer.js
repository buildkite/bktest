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
  onTestEnd(test, result) {
    this.attempts.push({ name: test.title, start: result.startTime.getTime(), steps: result.steps.map(describeStep) })
  }
  onEnd() {
    if (process.env.PLAYWRIGHT_JSON_OUTPUT_NAME) {
      fs.writeFileSync(process.env.PLAYWRIGHT_JSON_OUTPUT_NAME + '.steps', JSON.stringify(this.attempts))
    }
  }
}
