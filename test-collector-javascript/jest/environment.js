const { JestOtelEnvironment } = require('./otelEnvironment')

const extend = (Base) => class extends Base {
  static buildkiteTestEngine = true
  constructor(config, context) {
    super(config, context)
    this.collector = new JestOtelEnvironment(config, context)
    this._buildkite = this.collector._buildkite
  }
  handleTestEvent(event, state) {
    return this.collector.handleTestEvent(event, state, super.handleTestEvent?.bind(this))
  }
  async teardown() {
    try { await this.collector.teardown() } finally { await super.teardown() }
  }
}

const mod = require(require.resolve('jest-environment-node', { paths: [process.cwd(), __dirname] }))
module.exports = extend(mod.default || mod.TestEnvironment || mod)
module.exports.extend = extend
