module.exports = async () => {
  process.env[process.env.PLAYWRIGHT_OTEL_SETUP_TOKEN] = 'setup-token'
}
