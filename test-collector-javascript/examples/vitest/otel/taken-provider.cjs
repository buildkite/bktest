// Preloaded to simulate project BB's workers already having a tracer provider.
const { trace } = require('@opentelemetry/api')

const register = trace.setGlobalTracerProvider.bind(trace)
trace.setGlobalTracerProvider = (provider) => process.env.PROJECT_LABEL === 'BB' ? false : register(provider)
