import config from './vitest.config.js';

// Outside the default include, so `npm test` keeps uploading only the other
// examples. Retry is set here because every supported Vitest reads it from
// config, while per-test options moved between arguments across versions.
export default {
  ...config,
  test: {
    ...config.test,
    include: ['retries.example.js'],
    retry: 1,
  },
};
