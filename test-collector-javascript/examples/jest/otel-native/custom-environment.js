const fs = require('fs')
const { TestEnvironment } = require('jest-environment-node')

module.exports = class extends TestEnvironment {
  constructor(config, context) {
    super(config, context)
    this.report = config.projectConfig.testEnvironmentOptions.report
    this.events = []
  }
  async setup() {
    await super.setup()
    this.global.nativeSetup = true
    this.handleTestEvent = async function (event) { this.events.push(event.name) }
  }
  async teardown() {
    fs.writeFileSync(this.report, JSON.stringify({ setup: this.global.nativeSetup, events: this.events }))
    await super.teardown()
  }
}
