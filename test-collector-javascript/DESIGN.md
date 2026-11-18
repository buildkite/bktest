# Design

This document describes the experimental OpenTelemetry implementation. For setup
and supported behavior, see the [OpenTelemetry guide](docs/opentelemetry.md).

## Local spool

Jest records spans in workers and uploads from its main-process reporter.
At file teardown, workers write spans to private local spool files using Node's
V8 serializer and atomic renames. Child commits are separate, so a child
serialization or write failure warns without blocking test results. Workers
wait for local I/O, not the network. Commits are not fsync/power-loss durable.
The reporter owns the temporary directory and OTLP token.

Spools stay within the Node process tree and preserve structured attributes and
non-finite numbers. They are neither JSON uploads nor a cross-version interchange
format. See [spool.js](util/otel/spool.js) and [session.js](util/otel/session.js).

File-result callbacks register expected files without scanning the spool or
waiting for uploads. The parent scans on a one-second timer and at the final
snapshot. It reports files without readable commits once per reason, plus an
end-of-run file/test summary. An unreadable spool leaves recording status
unknown, rather than implying setup failed. With no credential, only the
configuration warning is printed. Worker/environment failures producing no test
results do not add collector warnings.

Repeated in-band runs in one process reuse the worker session with the new spool
directory. The run key remains process-scoped. Completed runs remove the spool
directory. Terminated runs may leave it behind, but there is no replay tool.

## Upload lanes

The parent rebuilds readable spans and uses one stock OpenTelemetry
`OTLPTraceExporter` per lane (test and child), with up to four exports in flight
per lane. It batches across files for up to one second, retaining span-count caps
and measuring with the stock protobuf serializer to enforce a 750 KiB
uncompressed protobuf cap. That leaves headroom below the endpoint's 900 KiB limit.

The stock exporter owns gzip, authorization/run-key/stream headers, 10-second
request timeouts and its retry policy. Its keep-alive Agent also has a 10-second
inactivity timeout starting before TCP connects, so a silently dropped connection
does not hold a slot until the OS timeout. Failed batches remain on disk and are
requeued during the run for network errors, HTTP 429 and 5xx, honoring
`Retry-After`. Other HTTP errors are reported without retries. Lost
acknowledgments can cause duplicates: delivery is not exactly once.

See [uploads.js](util/otel/uploads.js) and [exporter.js](util/otel/exporter.js).

### Child capacity and rejection

The parent holds at most 16,384 sendable pending child spans in memory. It admits
timestamp-ordered child files only when there is capacity. Excess files stay
unread on disk until acknowledgments or permanent failures free room. This cap
never excludes test spans and does not itself drop children. Workers split files
with a larger configured per-file child cap into admissible chunks. Child counts
in filenames allow final accounting without reading deferred payloads.

The first child-stream HTTP 403 stops new child uploads for the rest of the run.
Already in-flight requests can finish. The parent deletes rejected child files,
including later commits on subsequent scans, retaining drop counts in memory
instead of payloads on disk. It counts unread files from their filenames.
Workers still serialize and commit children until the run ends. The final
child-drop warning includes the server reason when available and the full total.
Test results continue uploading independently.

Other permanent child failures, such as HTTP 413, remain per batch. They release
payloads and capacity, and later children continue uploading. Child files are
removed once every part is acknowledged or permanently dropped. Oversized single
spans also release payloads and retain drop counts. Retryable failures retain
capacity until acknowledged or the drain ends.

Per-file cap drops and permanent upload failures use a single final warning,
not one per file or rejected batch. Child-spool failures, including late
completions, warn once per run and join that total when drop counts can be written.
If drop-count records cannot be written or read, a warning marks the total
incomplete or unavailable. Still-open children and children started after
teardown are reported separately once per worker at exit, outside the parent's total.

## Jest drain and shutdown

Normal completion and `--bail` share one memoized final drain over a fixed
snapshot, with a 10-second budget and no batching delay. The budget uses the same
constant as the stock exporter timeout. Healthy runs finish as soon as uploads
do. At the deadline, the drain stops waiting and aborts connecting and active
HTTP/HTTPS sockets through the Agent's signal option.

A stock-exporter retry already scheduled can keep Node alive for up to 10 more
seconds: `Retry-After` bypasses the usual five-second backoff cap. The retry fails
with `ABORT_ERR` without opening another socket. The drain budget is not an exact
process-exit deadline.

Bail gates new files through the reporter's public start callback. Already
running files can finish. SIGTERM gates new files too and shortens an existing
drain to at most five more seconds. The handler records whether other SIGTERM
listeners own shutdown before their once-handlers run, preserving their
asynchronous cleanup. Without another owner it re-raises SIGTERM, or exits 143
as PID 1. Files arriving after the snapshot do not join the drain.

Unacknowledged test results use `TEST RESULTS MISSING`. Unacknowledged children,
resident or unread on disk, use child-drop wording. Neither filesystem survival
nor an HTTP request reaching the server proves acknowledgment. SIGKILL can
prevent draining and warnings. A crash before a file commits loses its results.
Late-ending children started before teardown can commit separately. Children
started after their trace closes, or still open at worker exit, are dropped and counted.

See [Jest reporter](jest/reporter.js) and [worker](jest/otelWorker.js).

## Playwright lifecycle

The [reporter](playwright/reporter.js) commits completed attempts and children to
the spool for background uploads. Normal completion drains for up to 10 seconds.
`globalTimeout` can notify reporters before interrupted attempts finish. The
collector waits up to one second for callbacks, then records open attempts as
interrupted/skipped before taking the upload snapshot. Late callbacks do not
duplicate those attempts. This wait is within the drain budget.

Playwright itself does not treat SIGTERM as graceful interruption. The collector
handles it with a five-second budget including the attempt wait, and shortens
an existing drain. It preserves other SIGTERM listeners. Without another listener
it re-raises SIGTERM, or exits 143 as PID 1. Open attempts are skipped, with
unfinished children marked incomplete, not failed. Ctrl-C permits worker
interruption and flushing. A second interrupt, SIGKILL or a crash can prevent
both uploads and warnings.

Step errors use the same 10,243-code-point truncation as test exception messages.
API-title redaction replaces the whole quoted region because Playwright does
not escape embedded quotes. It also handles older parenthesized API arguments.
Redaction covers both the span name and `test.step.name`, including typed text,
pressed keys, option values and locator literals. Repeated titles remain separate
spans with Playwright's parent relationships and timings. Step errors mark the
child failed without changing the test result. Open steps close at the root's end
with `test.step.incomplete`. Test identity and failure details match JSON.

## Context and instrumentation

Jest does not replace a suite-owned tracer provider. Where none exists, the
collector installs a worker context manager and propagator, fills sandbox noop
context managers, and delegates sandbox tracer proxies to its provider. Cached
tracers follow a provider registered later by the suite. Test context is
non-recording, so test code cannot mutate the result span through
`trace.getActiveSpan()`. An attempt still open at teardown without errors gets
`unset`, which ingestion ignores without creating an execution.

See [otelContext.js](jest/otelContext.js). Uploads suppress their own tracing,
including in-band runs. The collector probes serialization before recording
results, since mixed SDK lines can create spans but fail serialization. SDK 3
on older Node versions warns and still attempts export.

Vitest loads packages by file path through Node's ES module loader. Each worker
registers import-in-the-middle for selected package entry points only. It uses
synchronous module hooks where supported by the installed import-in-the-middle,
and `module.register` otherwise. Entry-point patches such as Express/ioredis
cannot match file-path imports through import-in-the-middle up to at least 3.6.0.
GraphQL 16 has an extensionless `main` that Vitest resolves to `index.mjs`, while
Node loads the instrumented `index.js` build. These unsupported imports warn
once per package. Packages Vite runs itself through `server.deps.inline` never
reach Node's loader and cannot trigger that warning. See
[otelPackages.js](vitest/otelPackages.js).

Vitest VM pools cannot collect children because Vitest's `instanceof Promise`
check does not recognize the cross-realm Promise returned by `run.test`. It ends
the test span before the work finishes. Its SIGTERM logger exits before an
asynchronous drain can finish, so the reporter warns synchronously instead.

Instrumentation compatibility is determined from its base
`@opentelemetry/instrumentation` dependency. Instrumentations receive both tracer
and meter providers. Default exclusions include `lru-memoizer`, whose patch drops
memoizer methods without recording spans, and `router`, which duplicates Express
5 handler/middleware spans. See [instrumentations.js](util/otel/instrumentations.js).

## Result and attribute details

Jest supports concurrent tests, callbacks, generators, fake timers and module
resets. Results include hook failures. Durations use real time and stop when the
test finishes. Agent tracing links separate test traces to the job through
`TRACEPARENT` and `TRACESTATE`. Native ESM with API 1.10 supports tracers obtained
before `resetModules` and synchronous `require(esm)`.

SDK 3 structured span/link attributes survive export: bytes, nested maps, mixed
arrays and null. Event/resource attributes remain limited to simple SDK values.
SDK validation warnings go to OpenTelemetry's `diag` logger.
At DEBUG, that logger also receives selected instrumentation names once per file.

Vitest records skips and tests cancelled by bail. Hook phases retain Vitest names,
statuses and exceptions, but only export when they group an exported child, not
for errors or events alone. Starting a child without ending it does not export
otherwise-empty phases. Without instrumentation or manual spans, passing and
failing suites send no child-stream requests. Test spans still record failures.
HTTP/fetch calls outside tests bypass instrumentation even with active parent
context, while servers started outside tests can trace requests from tests.
