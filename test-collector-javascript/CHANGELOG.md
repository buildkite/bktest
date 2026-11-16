# Changelog

## 1.15.0

- Support OpenTelemetry JS SDK 3 (`@opentelemetry/sdk-trace` 3.x, API 1.10+, Node.js 22.15+) in Jest, Vitest and Playwright. SDK 2.9+ still works, and child spans keep environment-selected sampling and limits.
- **Migration:** OTel mode now loads `@opentelemetry/sdk-trace` 2.9+ instead of `sdk-trace-base`. Projects on SDK 2.0–2.8 or Yarn Plug'n'Play must add it, or the collector warns and uploads nothing. Custom providers use `TracerProvider`, and processors take `{ exporter }`. See the [setup guide](docs/opentelemetry.md#setup-1).
- Preserve SDK 3 structured span/link attributes and Jest native ESM manual spans. SDK 3 rejects nested plain objects created inside Jest tests; use `Object.create(null)` instead.
- Keep test results when child spans fail. A child-stream HTTP 403 stops child uploads for the rest of the run and shows the server's reason. Child serialization and spool-write failures only warn.
- Playwright: redact API input values from child span names, keep results on SIGTERM and global timeouts (interrupted attempts report as skipped), support 1.41.2, and truncate large step errors.
- Vitest: stop sending billed child spans for uninstrumented suites, send execution spans only for VM pools, warn on SIGTERM instead of attempting a drain, and warn when a Vitest release changes its spans so children can't be linked to tests.
- Fall back to `BUILDKITE_ANALYTICS_TOKEN` when the reporter token is blank.

## 1.14.0

- Add experimental OpenTelemetry mode for Playwright. Each test attempt becomes a span, with `test.step` blocks, hooks, fixtures, assertions and actions as child spans. Enable it with reporter option `otelEnabled: true`; see the [Playwright guide](docs/opentelemetry.md#playwright).
- Add experimental OpenTelemetry mode for Vitest 4.0.11+. See hooks, manual spans and HTTP/fetch calls in a trace for each test. Retries share that trace. Follow the [setup guide](docs/opentelemetry.md#vitest) to enable it.
- JSON uploads remain the default. OpenTelemetry mode does not fall back to JSON on failure.
- When a span is too large to upload, the dropped-span summary now says so instead of blaming the drain deadline.

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
