# Changelog

## 1.14.0-beta.1

- Add experimental Vitest OpenTelemetry mode using Vitest's native tracing (4.0.11+). Enable `otelEnabled: true` on the reporter and point `experimental.openTelemetry.sdkPath` at `buildkite-test-collector/vitest/otel.js`; see the [guide](docs/opentelemetry.md#vitest).
- Report one execution per test, with retries, hook phases, manual spans and HTTP/fetch children. Empty phases, suite work and `beforeAll` are excluded. Skipped suites and bail-cancelled tests still get executions.
- Reuse Jest's SDK 2.x gate, instrumentation discovery, spool, uploads and child limits. Vitest supports http/undici instrumentation, not package instrumentation or browser children. Vitest 3 and user-owned SDKs warn and send test spans only; Vitest 1–2 warn and upload nothing. No JSON fallback.

## 1.13.0

- Send Jest results to Test Engine as OpenTelemetry spans, with HTTP calls, database queries and manual spans underneath. Enable this experimental mode with reporter option `otelEnabled: true` and `runtime: 'buildkite-test-collector/jest/runtime'`. JSON uploads remain the default.
- Use Jest 29.7.0 or 30.x with `jest-circus`, OpenTelemetry SDK 2.x and exporter 0.200–0.2xx on Linux or macOS. Windows is untested. OpenTelemetry packages are optional; existing JSON users don't need them.
- In OpenTelemetry mode, configuration or setup problems warn and upload nothing. There is no JSON fallback: fix the warning and rerun, or turn OpenTelemetry off to use JSON.
- Upload from the parent process so test files no longer wait for the network. Retry temporary failures during the run. Missing test results are reported as `TEST RESULTS MISSING`.
- Allow ten seconds for uploads at completion; healthy runs finish as soon as uploads do. During an outage, an already-scheduled retry can keep Jest alive for up to ten more seconds. SIGTERM allows a five-second drain; SIGKILL loses results not yet uploaded. Interrupted results cannot be replayed.
- Count each test attempt as a test execution at no extra charge. Child spans are billed by data ingested and require a plan that includes them. Dropped children are counted and warned about separately; permanent child failures don't block later children, and test results are unaffected by child losses. Child spans can contain URLs, query strings and database statements.
- For beta users, replace `testEnvironment: 'buildkite-test-collector/jest/environment'` with `runtime` and keep your usual test environment.
- Batch-size environment variables now limit parent uploads, with a default and maximum of 1,000 test spans, also bounded by 750 KiB uncompressed. `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` now caps children per file; `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` is ignored.
- Read the [OpenTelemetry guide](https://github.com/buildkite/bktest/blob/main/test-collector-javascript/docs/opentelemetry.md) for installation, instrumentation, billing and delivery limits. The OpenTelemetry feature remains experimental in 1.13.0.
