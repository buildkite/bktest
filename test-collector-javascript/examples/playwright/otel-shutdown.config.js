module.exports = {
  ...require('./otel.config'),
  testMatch: 'shutdown.js',
  workers: 1,
  globalTimeout: process.env.PLAYWRIGHT_OTEL_SIGNAL ? 0 : 4000,
}
