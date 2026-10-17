// The OpenTelemetry packages are optional peer dependencies: they are only
// required here, once OTel mode has been requested and the credential and run
// key have been accepted, never at package load.
const loadSdk = () => ({
  api: require('@opentelemetry/api'),
  trace: require('@opentelemetry/sdk-trace-base'),
  resources: require('@opentelemetry/resources'),
  exporter: require('@opentelemetry/exporter-trace-otlp-proto'),
})

module.exports = { loadSdk }
