const config = require('./otel.config')

module.exports = {
  ...config,
  reporter: config.reporter.map(([name, options]) => [name, { ...options, otelEnabled: false }]),
}
