# Changelog

## 1.13.0-beta.5

- Add experimental OpenTelemetry export for Jest 29.7.0 and 30.x. Set `otelEnabled: true` on the reporter and add `buildkite-test-collector/jest/runtime`. Keep your usual test environment. JSON uploads remain the default.
- Workers atomically commit exporter-serialized OTLP protobuf to a per-run local spool at file teardown, without network waits. The parent reporter batches and uploads separate test and child streams, with up to four requests in flight per stream and a 750 KiB uncompressed protobuf cap.
- With `otelEnabled: true`, never upload JSON, including on unsupported Jest/SDK versions, invalid settings, missing runtime or worker setup failure. Warn with remediation instead. Worker settings no longer contain the OTLP token.
- Retry network errors, 429 and 5xx through the run, honoring `Retry-After`. Final completion uses one fixed-snapshot drain with a five-second budget; bail does not dispatch extra files while it drains. SIGTERM drains for up to five seconds before exiting with the signal. Report unacknowledged counts as `TEST RESULTS MISSING`. SIGKILL loses files not yet uploaded; there is no replay tool.
- Reinterpret `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` as a per-file child cap (including open spans), counting drops. The test/child batch-size variables now cap parent uploads; `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` is ignored because test spans no longer enter a bounded SDK queue.
- Keep unfinished error-free test attempts `unset`, not passed, matching the Ruby collector. Preserve once-per-run instrumentation warnings across workers using the spool directory.
- Document experimental Linux/macOS support (Windows untested) and the optional bktec relay. Its Go default transport reads HTTP(S) proxy settings; that behavior was inspected in code, not tested end to end.
- Test spans count as one execution per attempt at no extra charge. Child spans can contain URLs, query strings and database statements. They are billed by data ingested and need a plan that includes them.
- OpenTelemetry packages are optional. Read the [OpenTelemetry guide](https://github.com/buildkite/bktest/blob/main/test-collector-javascript/docs/opentelemetry.md) for installation, billing and limits.

## 1.13.0-beta.4

- Set up OpenTelemetry with the reporter and runtime, and keep your own test environment. The beta `jest/environment` setup is gone: use your usual environment and add `runtime`.
- Installing no longer fails next to older OpenTelemetry packages, such as those from `@sentry/node` 8 or 9.
- Mocking `@opentelemetry/api`, or app code changing the active span, no longer changes test results.
- Limit test batches to 200 spans, preserve job links despite `OTEL_SPAN_*` settings, and flush test and child spans together.
- Type-check reporter options in `jest.config.ts` with `jest/reporter.d.ts`.
- Send selected instrumentation names to your OpenTelemetry `diag` logger.
- Keep beta.2's error stacks when no package instrumentation is selected.
- Simplify setup instructions, settings and troubleshooting.

## 1.13.0-beta.3

- Select project/ancestor-declared 2.x instrumentations and resolvable auto-instrumentations-node bundles, not incidental hoisted dependencies.
- Keep filesystem, log/message-changing, low-level, metrics-only and browser instrumentations opt-in; standard `OTEL_NODE_*` selection applies, with disabled names winning.
- Respect each project's selection in shared workers without duplicate HTTP/fetch spans or unwanted trace headers.
- Recover HTTP/HTTPS instrumentation after interceptors restore built-ins.
- Skip package patches with one warning for mocked/incomplete APIs; test results still export.
- Preserve unchanged getter-only module exports, including Fastify dependencies.
- Export already-started children after teardown; count new late descendants and unfinished children in dropped-span warnings.
- Document compatibility, span counts/billing and opt-outs; the existing debug switch reports selected names once per worker.
