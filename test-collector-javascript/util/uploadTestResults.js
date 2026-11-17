const Debug = require('../util/debug')
const axios = require('axios')
const workerIdTag = require('./workerIdTag')

const CHUNK_SIZE = 5000
const DEFAULT_BUILDKITE_ANALYTICS_BASE_URL = 'https://analytics-api.buildkite.com/v1/uploads'

const uploadTestResults = (env, tags, results, options, done) => {
  const buildkiteAnalyticsToken = options?.token || process.env.BUILDKITE_ANALYTICS_TOKEN
  const buildkiteAnalyticsUrl = options?.url || process.env.BUILDKITE_ANALYTICS_BASE_URL || DEFAULT_BUILDKITE_ANALYTICS_BASE_URL

  if (!buildkiteAnalyticsToken) {
    console.error('Missing BUILDKITE_ANALYTICS_TOKEN')

    if (done !== undefined) { return done() }
    return
  }

  // Transport errors can include the authenticated request in their message.
  const redact = message => message.split(buildkiteAnalyticsToken).join('[REDACTED]')

  const config = {
    headers: {
      'Authorization': `Token token="${buildkiteAnalyticsToken}"`,
      'Content-Type': 'application/json'
    }
  }

  const requests = [];
  const client = axios.create();

  if (Debug.enabled()) {
    client.interceptors.request.use(function (config) {
      const { method, url, data } = config;
      Debug.log(redact(`Test Engine Sending: ${JSON.stringify({ method, url, data })}`));
      return config;
    }, function (error) {
      if (error.response) {
        Debug.log(redact(`Test Engine request error: ${error.response.status} ${error.response.statusText} ${JSON.stringify(error.response.data)}`));
      } else {
        Debug.log(redact(`Test Engine request error: ${error.message}`))
      }
      // Do something with request error
      return Promise.reject(error);
    });
  }

  // Add a response interceptor
  client.interceptors.response.use(function (response) {
    // Any status code that lie within the range of 2xx cause this function to trigger
    // Do something with response data
    Debug.log(redact(`Test Engine success response ${JSON.stringify(response.data)}`));
    return response;
  }, function (error) {
    if (error.response) {
      console.log(redact(`⚠️ Test Engine error response: ${error.response.status} ${error.response.statusText} ${JSON.stringify(error.response.data)}`));
    } else {
      console.log(redact(`⚠️ Test Engine error: ${error.message}`))
    }
    return Promise.reject(error);
  });

  for (let i = 0; i < results.length; i += CHUNK_SIZE) {
    const data = {
      'format': 'json',
      'run_env': env,
      'tags': { ...workerIdTag(), ...(tags || {}) },
      "data": results.slice(i, i + CHUNK_SIZE),
    }

    requests.push(client.post(buildkiteAnalyticsUrl, data, config))
  }

  return Promise.allSettled(requests)
    .finally(function (responses) {
      if (done !== undefined) { return done() }
    })
}

module.exports = uploadTestResults
