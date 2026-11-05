# Changelog

## 1.13.0-beta.5

- Add experimental OpenTelemetry export for Jest 29.7.0 and 30.x. Set `otelEnabled: true` on the reporter and add `buildkite-test-collector/jest/runtime`. Keep your usual test environment. JSON uploads remain the default.
- Workers atomically commit plain span data as local JSON to a per-run spool at file teardown, without network waits. Test and child streams use separate files; test commits include the Jest project ID so one project's commit cannot hide another project's missing results. The parent restores readable spans and uploads through the stock OpenTelemetry exporter in separate test and child streams, with up to four exports in flight per stream and a serializer-measured 750 KiB uncompressed protobuf cap.
- Bound resident pending children to 16,384 spans, leaving excess child files unread on disk until acknowledgments free capacity. Count unacknowledged children from both memory and disk at drain completion, without parsing deferred files. Test spans are never excluded by this cap. Preserve NaN and positive/negative Infinity across the local JSON boundary, including arrays and span/event/link/resource attributes.
- Release child payloads and resident capacity after permanent upload failures or an oversized single span, while retaining their counts for the final dropped-child warning. Continue admitting later child files, including after HTTP 403 or 413; retryable failures still occupy capacity until acknowledged or the drain ends.
- With `otelEnabled: true`, never upload JSON, including on unsupported Jest/SDK versions, invalid settings, missing runtime or worker setup failure. Warn with remediation instead. Worker settings no longer contain the OTLP token.
- Retry network errors, 429 and 5xx through the run, honoring `Retry-After`. Final completion uses one fixed-snapshot drain with a 10-second budget, sharing the stock exporter timeout constant; bail does not dispatch extra files while it drains. Healthy runs finish as soon as uploads complete. In an outage, Jest normally exits about ten seconds after tests finish, with up to ten additional seconds in rare cases for a stock retry already scheduled. SIGTERM still drains for up to five seconds before exiting with the signal. Report unacknowledged counts as `TEST RESULTS MISSING`. SIGKILL loses files not yet uploaded; there is no replay tool.
- Reinterpret `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` as a per-file child cap (including open spans), counting drops. The test/child batch-size variables now cap parent uploads; `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` is ignored because test spans no longer enter a bounded SDK queue.
- Raise the default and maximum test batch size to 1,000 spans to reduce dense-suite upload latency. The 750 KiB uncompressed protobuf cap still limits each request, leaving headroom below ingestion limits. Child batches remain at 512 and each lane still allows four exports in flight.
- Keep unfinished error-free test attempts `unset`, not passed, matching the Ruby collector. Preserve once-per-run instrumentation warnings across workers using the spool directory.
- Restore bounded warnings (one per missing-file reason and one final file/test summary; only the configuration warning without credentials), child-only drop wording, and repeated in-band runs with a mutable spool path. Scan on the parent timer rather than on every file result. Preserve other SIGTERM handlers, including once-listeners performing asynchronous cleanup, and exit with code 143 as PID 1 only when no other handler owned shutdown at signal delivery.
- Distinguish missing-file reasons (no runtime, jasmine, setup failure, unusable settings or an unreadable spool) from unacknowledged committed spans. An unreadable spool leaves recording status unknown rather than implying setup failed.
- Set a 10-second Agent inactivity timeout as well as the stock exporter timeout, so blackholed TCP connects release lane slots during the run. At drain completion, abort each lane's sockets through the Agent's signal option, including pending HTTP/HTTPS connections. This works with exporter 0.200.0 on Node 18.19.0 and keeps `keepAlive: true` explicitly.
- A stock-exporter retry already scheduled at drain completion can keep Node alive for up to the ten-second export retry budget (`Retry-After` can exceed the usual five-second backoff cap). Reject that retry with `ABORT_ERR` without opening another socket; the drain budget is not an exact process-exit deadline.
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
