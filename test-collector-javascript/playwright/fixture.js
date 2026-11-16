const { test: base, expect } = require('@playwright/test')
const CI = require('../util/ci')
const { credentialFor, isValidEndpoint, isValidRunKey, otlpEndpoint } = require('../util/otel/config')
const { warnOncePerRun, describeError } = require('../util/otel/warn')
const { testIds, currentRun } = require('./otelIds')

// Continues the reporter's test.execution span in the app under test: requests
// carry a W3C traceparent whose parent is that span. Without an OTel-mode
// reporter there is no run seed, and nothing changes.
const traceparentFor = (run, testInfo) => {
  const { traceId, spanId } = testIds(run.seed, testInfo.testId, testInfo.retry)
  return `00-${traceId}-${spanId}-01`
}

const appOrigins = (origins, baseURL) => {
  const configured = origins?.length ? origins : baseURL ? [baseURL] : []
  return new Set(configured.map(origin => new URL(origin).origin))
}

const appTracingFixtures = {
  buildkiteAppOrigins: [[], { option: true }],
  // Static assets would multiply billed spans without explaining test time.
  buildkiteAppResourceTypes: [['document', 'fetch', 'xhr'], { option: true }],

  context: async ({ context, baseURL, buildkiteAppOrigins, buildkiteAppResourceTypes }, use, testInfo) => {
    const run = currentRun()
    if (run) {
      try {
        const origins = appOrigins(buildkiteAppOrigins, baseURL)
        if (origins.size === 0) {
          warnOncePerRun(run.spool, 'app-origins', 'App tracing is off: set use.baseURL or use.buildkiteAppOrigins to the app\'s origins.')
        } else {
          const traceparent = traceparentFor(run, testInfo)
          const types = new Set(buildkiteAppResourceTypes)
          await context.route(url => origins.has(url.origin), route => {
            const request = route.request()
            const headers = request.headers()
            // Playwright reapplies overrides after redirects. Leave auth alone
            // so Chromium can strip it when the redirect crosses origins.
            return types.has(request.resourceType()) && !('authorization' in headers)
              ? route.fallback({ headers: { ...headers, traceparent } })
              : route.fallback()
          })
        }
      } catch (error) {
        warnOncePerRun(run.spool, 'app-route', `App tracing is off for this test: ${describeError(error)}`)
      }
    }
    await use(context)
  },

  request: async ({ request }, use, testInfo) => {
    const run = currentRun()
    if (!run) return use(request)
    const traceparent = traceparentFor(run, testInfo)
    await use(new Proxy(request, {
      get(target, key) {
        const value = Reflect.get(target, key, target)
        if (typeof value !== 'function') return value
        if (!['get', 'post', 'put', 'patch', 'delete', 'head', 'fetch'].includes(key)) return value.bind(target)
        return (url, options = {}) => {
          const headers = options.headers || (typeof url === 'string' ? {} : url.headers())
          return value.call(target, url, {
            ...options,
            headers: Object.keys(headers).some(name => name.toLowerCase() === 'traceparent')
              ? headers
              : { ...headers, traceparent },
          })
        }
      },
    }))
  },
}

const header = (name, value) => `${name}=${encodeURIComponent(value)}`

// Environment for the app under test, for example webServer.env. The app's own
// OTel SDK reads it and exports spans that continue a sampled remote parent.
const appTracingEnv = ({ token } = {}) => {
  const credential = credentialFor(token)
  const endpoint = otlpEndpoint()
  const env = new CI().env('playwright')
  if (!credential || !isValidRunKey(env.key) || !isValidEndpoint(endpoint)) return {}
  // Outside CI the run key is random per call: pin it for the reporter.
  if (env.ci === 'generic' && !process.env.BUILDKITE_ANALYTICS_KEY) process.env.BUILDKITE_ANALYTICS_KEY = env.key
  return {
    OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: endpoint,
    OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: 'http/protobuf',
    OTEL_EXPORTER_OTLP_TRACES_COMPRESSION: 'gzip',
    OTEL_EXPORTER_OTLP_TRACES_HEADERS: [
      header('authorization', `Bearer ${credential}`),
      header('buildkite-tests-run-key', env.key),
      header('buildkite-tests-span-stream', 'child'),
    ].join(','),
    OTEL_TRACES_SAMPLER: 'parentbased_always_off',
    OTEL_BSP_SCHEDULE_DELAY: '1000',
  }
}

const test = base.extend(appTracingFixtures)

module.exports = { test, expect, appTracingFixtures, appTracingEnv }
