# OpenTelemetry (experimental)

Test Engine can receive your Jest and Playwright results as OpenTelemetry traces.
Each test becomes a span. With Jest, the work inside it (HTTP calls, database queries, your own spans) shows up underneath.

## Playwright

Install the dependencies in [Setup](#setup), then enable `otelEnabled` on the
reporter. No fixture or test changes are needed, and Jest's runtime is not required.

```js
// playwright.config.js
module.exports = {
  reporter: [
    ['line'],
    ['buildkite-test-collector/playwright/reporter', { otelEnabled: true }],
  ],
}
```

Each test attempt becomes a `test.execution` span, including retries and skips,
starting at Playwright's reported start and extending to enclose all observed
steps. Test Engine's execution duration can therefore exceed Playwright's JSON
duration, including suite hooks and setup outside Playwright's test timer.
Reporter and upload latency are not added. Test identity and failure details match JSON reporting.
Interrupted attempts are reported as skipped; expected failures remain failures,
as in JSON mode.

Playwright steps become nested child spans: `test.step` blocks, hooks, fixtures,
assertions and Playwright actions. Repeated titles remain separate spans, with
Playwright's parent relationships and start/duration timings. Step errors mark
the child as failed without changing the test result. Steps left open when an
attempt ends are marked `test.step.incomplete` and closed at the root span's end;
cancellation does not make them failures.

Reporter `token` and `tags` options and the shared [settings](#settings) apply.
Per-test `@key:value` tags override reporter tags. Child export limits apply per
file across workers and retries; dropped children do not displace test results.

With OTel enabled, configuration or upload failures warn without falling back to
JSON or changing Playwright's exit code. Turn `otelEnabled` off to use JSON again.

This is reporter-side tracing only. It does not propagate context into workers
or collect automatic HTTP/database spans, manual worker spans, browser application
traces or in-test annotations. Ctrl-C allows results to flush; SIGTERM can lose
pending results.

## Jest

Jest runs tests in worker processes and reporters in the main process.
The runtime records spans in the workers; the reporter uploads them.

```js
// jest.config.js
module.exports = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', { otelEnabled: true }],
  ],
  runtime: 'buildkite-test-collector/jest/runtime',
  testLocationInResults: true,
}
```

## Setup

```bash
npm install --save-dev buildkite-test-collector \
  '@opentelemetry/sdk-trace-base@^2' \
  '@opentelemetry/context-async-hooks@^2' \
  '@opentelemetry/exporter-trace-otlp-proto@<0.300'
```

With pnpm or Yarn, also add `@opentelemetry/api@~1.9.0`.
Under Yarn Plug'n'Play, declare these packages in the root workspace.

Use Jest 29 or 30 with `jest-circus` (tested with 29.7.0 and 30.5.2). OpenTelemetry SDK 2 needs Node.js 18.19+ or 20.6+.
Jest 30.0 and 30.1 can drop `afterEach` children in files mixing concurrent and sequential tests.
It's experimental and off by default.
Supported platforms are Linux and macOS; Windows is untested.
OpenTelemetry options, environment variables and span shape may change in minor releases.
Test spans count as test executions, like JSON uploads, at no extra charge. Child spans are billed by data ingested and need a plan that includes them; without it, we still record test results and warn that child spans were dropped.

Install the versions above. The collector checks SDK 2 and exporter 0.200–0.2xx compatibility, including a serialization probe. SDK 3 is unsupported; API package versions are not independently gated.

Reporter `token` and `tags` options work as usual.
For TypeScript, check your reporter options with `satisfies Options`, importing `Options` from `buildkite-test-collector/jest/reporter`.

Upgrading from a 1.13.0 beta? Remove `testEnvironment: 'buildkite-test-collector/jest/environment'` and add `runtime`. Batch-size variables now limit parent uploads, the child queue-size variable caps children per file, and the test queue-size variable is ignored; see [Settings](#settings).
With `otelEnabled: true`, every project needs `runtime`. A project without it warns and uploads nothing. There is **no JSON fallback**, including for unsupported Jest/SDK versions, invalid settings, worker setup failures, or an unwritable temporary directory. Fix the warning and rerun. To use JSON instead, turn `otelEnabled` off for the run.
A `@jest-environment` docblock doesn't opt a file out.
For a custom runtime, use `require('buildkite-test-collector/jest/runtime').extend(MyRuntime)`.

The parent reporter uploads, not the workers. Worker HTTP mocks do not intercept uploads. In-band tests or parent-process mocks can still intercept them; if you use nock's `disableNetConnect` there, allow the collector's POST:

```js
nock.enableNetConnect(/tests-otlp\.buildkite\.com|127\.0\.0\.1/)
```

With MSW, bypass that request. A mock answering it makes results disappear without a warning.

## Settings

Set a suite API token, or an agent OIDC token that your suite's OIDC policy grants `write_uploads`.
The run key is the ID that groups results from one test run. It must be 1–255 printable ASCII characters without spaces.

| Variable | What it does | Default |
| --- | --- | --- |
| `BUILDKITE_ANALYTICS_TOKEN` | Authenticates uploads. Reporter `token` takes priority | Not set |
| `BUILDKITE_TESTS_OTLP_TOKEN` | Overrides the token for OpenTelemetry only | Uses the suite token |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | Sets an HTTP or HTTPS destination | `https://tests-otlp.buildkite.com/v1/traces` |
| `BUILDKITE_ANALYTICS_KEY` | Sets the run key | Derived from CI, or generated locally |
| `BUILDKITE_ANALYTICS_DEBUG_ENABLED` | Prints selected instrumentation and JSON upload debug output | `false` |
| `OTEL_NODE_ENABLED_INSTRUMENTATIONS` | Selects only these comma-separated short names | Declared packages, with exclusions below |
| `OTEL_NODE_DISABLED_INSTRUMENTATIONS` | Removes names, even from the enabled list | None |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` | Maximum test spans in a parent upload (at most 1,000), also capped at 750 KiB uncompressed protobuf | 1,000 |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` | Maximum child spans in a parent upload | 512 |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` | Child spans accepted per test file, including still-open spans. Further children are dropped and counted | 2,048 |
| `OTEL_RESOURCE_ATTRIBUTES` | Adds resource attributes to every span | None |

OTLP export doesn't use `HTTP_PROXY` or `HTTPS_PROXY`. JSON uploads honour them. Use [bktec's relay](#bktecs-relay) for OTLP through a proxy.
If you route OTLP through your own endpoint, forward `Authorization`, `Buildkite-Tests-Run-Key` and `Buildkite-Tests-Span-Stream`.
We ignore `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*` and `OTEL_TRACES_SAMPLER` for test results. Children use the environment's sampler and span limits, not a suite provider's code-configured sampler. `OTEL_BSP_*` does not control either stream.
Sizes must be positive whole numbers (at most 1,000 for test batches). Both streams also have a 750 KiB uncompressed protobuf cap; large spans reduce how many fit in one upload. Invalid values warn and use defaults. The old `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` is ignored: test results are no longer fed through a bounded SDK queue.
An OpenTelemetry `diag` logger at DEBUG receives selected names once per test file. `OTEL_LOG_LEVEL=DEBUG` alone doesn't install a logger.

### bktec's relay

The relay is optional. It is useful if you already run tests through `bktec run` (3.5.0 or later), or your CI needs an HTTP(S) proxy. Enable it with:

```sh
export BUILDKITE_TESTS_OTLP_RELAY=true
export BUILDKITE_TESTS_OTLP_RELAY_BATCHING=true
```

The relay sets the endpoint and token for the collector, acknowledges each export locally, and retries delivery in the background. Collector 1.13.0 removes per-file network waits even without the relay.
The relay uses Go's default HTTP transport, which reads `HTTP_PROXY` and `HTTPS_PROXY`. This proxy behavior was checked in bktec's code, not tested end to end.
Keep bktec's own upload off (`BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false`).

## What gets recorded

```js
const { trace } = require('@opentelemetry/api')
test('does work', async () => {
  await trace.getTracer('my-suite').startActiveSpan('work', async (span) => {
    try { await doWork() } finally { span.end() }
  })
})
```

If your suite registers its own tracer provider, it keeps its own child spans and we send test spans only.
To collect children in Buildkite, skip your application's provider setup when `JEST_WORKER_ID` is set.

- Every test gets a `test.execution` span, including skipped and todo tests. Each retry attempt gets a separate span and execution ID.
- Results keep names, locations, failures, tags and CI details. Durations use real time and stop when the test finishes.
- Tests have separate traces. The Agent with tracing enabled links them to the job using `TRACEPARENT` and `TRACESTATE`.
- Children belong to tests, `beforeEach` and `afterEach`, not file loading, `beforeAll` or `afterAll`.
- Concurrent tests, callbacks, generators, fake timers and module resets work. Results include hook failures.
- We always sample test results and never trace our own uploads, including in band. We do not replace a suite-owned provider. Where none exists, the collector installs a worker context manager and propagator, fills sandbox noop context managers, and delegates sandbox tracer proxies to its provider. Cached tracers follow a provider registered later by the suite.
- An attempt still open at file teardown without errors retains the `unset` result, not `pass` (matching the Ruby collector's unknown-result convention). Such an attempt does not become a passed execution in Test Engine.

`trace.getActiveSpan()` in a test is non-recording. It carries the parent context for children, but changing or ending it doesn't change the test result.
Child spans can contain URLs, query strings and database statements. Check what your instrumentation records before enabling it.
OpenTelemetry results don't yet count toward auto-quarantine or tag-to-label mappings, and don't update a test's file location.

## Choosing instrumentation

```bash
npm install --save-dev '@opentelemetry/instrumentation-http@>=0.200.0 <0.300.0' \
  '@opentelemetry/instrumentation-undici@>=0.11.0 <0.34.0'
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici npx jest
```

We select instrumentation declared in your Jest `rootDir`'s package.json or its parents, including workspace roots (under Yarn Plug'n'Play, the workspace that runs Jest).
If `@opentelemetry/auto-instrumentations-node` is resolvable, we select its whole bundle, with the exclusions below.
Individual packages installed incidentally by another dependency don't opt you in; a resolvable auto-instrumentations bundle does.
Use the SDK 2.x line: HTTP 0.200–0.2xx and undici 0.11–0.33. Other instrumentations are checked against their base instrumentation dependency (0.200–0.2xx), not a fixed auto-instrumentations bundle version range.
Incompatible instrumentation warns and is skipped without affecting results or manual spans.

We leave logs and messages (`pino`, `winston`, `bunyan`, `aws-sdk`, `kafkajs`, `amqplib`), low-level I/O (`net`, `dns`, `fs`) and metrics (`host-metrics`, `runtime-node`) off by default.
Browser instrumentation (`fetch`, `xml-http-request`, `document-load`, `user-interaction`, `long-task`) is also off. Use `undici` for Node's fetch.
You can opt in by listing short names in `OTEL_NODE_ENABLED_INSTRUMENTATIONS`. The disabled list always wins.
We test HTTP, fetch, Express 4, GraphQL, Redis and pg on Jest 29.7.0 and 30.5.2. Test other packages against your own operations before relying on them.

## Delivery and interruptions

At file teardown the worker writes plain span data as JSON to private local spool files and atomically renames each before returning. Children are committed separately before the test commit, which carries the file path and Jest project ID for accounting. This waits for local I/O only, not the network; it is not fsync/power-loss durability. The reporter creates the per-run temporary directory. Its worker settings contain no OTLP token. This JSON is only a local transport envelope, never a JSON results upload. Non-finite numeric attributes (NaN and positive/negative Infinity) are encoded and revived losslessly, including inside arrays.

The parent scans committed files on its one-second timer, rebuilds readable spans and uploads through one stock OpenTelemetry `OTLPTraceExporter` per lane (test and child), with up to four exports in flight per lane. It batches across files for up to one second, retaining span-count caps and measuring with the stock protobuf serializer to enforce a **750 KiB uncompressed protobuf** cap per request, leaving headroom below the endpoint's 900 KiB limit. The stock exporter owns gzip, the authorization/run-key/stream headers, 10-second request timeouts and its retry policy. Its Agent also has a 10-second inactivity timeout that starts before TCP connects, so a silently dropped connection does not occupy a lane slot until the OS timeout; keep-alive remains enabled. Failed batches remain on disk and are requeued through the run for network errors, 429 and 5xx, honoring `Retry-After`. Other HTTP errors are reported without retries. Lost acknowledgments can cause duplicates; delivery is not exactly once.

The parent holds at most 16,384 sendable pending child spans in memory. It admits timestamp-ordered child files only when there is capacity; excess files stay unread on disk until acknowledgments or permanent failures free room. Permanent child failures (including HTTP 403 or 413) and oversized single spans release their payloads and capacity, retaining counts for the final dropped-child warning without replaying them. Later child files continue uploading; test results are unaffected. Retryable failures retain capacity until acknowledged or the drain ends. This cap never excludes test spans and does not itself drop children. Child counts in filenames allow final accounting without loading deferred payloads. Files with a larger configured per-file child cap are split into admissible chunks by the worker.

Normal completion and `--bail` share one memoized final drain over a fixed snapshot, with a 10-second budget (the same constant as the stock exporter's timeout) and no batching delay. Healthy runs stop waiting as soon as uploads finish. In an outage, Jest normally exits about ten seconds after the tests finish. The drain stops waiting at its deadline and aborts each lane's connecting and active HTTP/HTTPS sockets through the Agent's signal option, supported even by exporter 0.200.0 on Node 18.19.0. In rare cases, a stock-exporter retry already scheduled can keep Node alive for up to ten additional seconds: `Retry-After` bypasses the usual five-second backoff cap. That retry fails with `ABORT_ERR` without opening another socket. The drain budget is not an exact process-exit deadline. Bail gates new files through the reporter's public start callback, so awaiting uploads does not keep dispatching files. Already running files can finish. SIGTERM gates new files too and shortens any existing drain to at most five more seconds. The collector snapshots other SIGTERM listeners before their once-handlers run, preserving their asynchronous cleanup; it re-raises SIGTERM (or exits with code 143 as PID 1) only if no other listener owned shutdown at signal delivery. Files arriving after the snapshot are not added to the drain. Unacknowledged test results are reported as `TEST RESULTS MISSING`; all unacknowledged children, whether resident or unread on disk, use child-drop wording and test results are unaffected by child losses. No JSON results are sent. Completed runs remove their spool directory; terminated runs may leave it behind. There is no replay tool.

File-result callbacks only register expected files; they never scan the spool or wait for uploads. The parent scans on its one-second timer and at the final snapshot, and reports files without readable commits once per reason plus an end-of-run file/test summary. If the spool is unreadable, recording status is unknown: the summary does not claim setup failed. With no credential only the configuration warning is printed. Suites with no test results after a worker/environment failure do not add collector warnings. Repeated in-band runs in one process reuse the worker session with the new spool directory (the run key remains process-scoped).

SIGKILL can prevent the drain and warning and **loses files not yet uploaded**. A crash/interruption before a file commits also loses that file's results. Late-ending children that started before teardown can commit separately; children started after their trace closed are dropped and counted. Children still open when a worker exits are counted as dropped. Neither filesystem survival nor an HTTP request reaching the server proves remote acknowledgment.

## Troubleshooting and known limits

- **No spans:** check the reporter, runtime, SDK versions, token, endpoint, temporary directory permissions/free space and earlier warnings, and use Jest 29 or 30 with `jest-circus`. Fix and rerun; there is no JSON fallback.
- **Missing children:** check your plan includes child spans. Check instrumentation warnings and mocked APIs, skip your own provider setup under Jest, and supply fetch yourself in jsdom if needed.
- **Duplicates:** retries after a lost acknowledgment can duplicate executions until the server deduplicates them.
- **Changed stacks:** package instrumentation adds collector frames to errors created while a module loads. Failing tests show context-manager frames whenever runtime is set. Use explicit labels if test names or snapshots include stacks.
- **Changed headers or messages:** disable the affected instrumentation using the table above if propagation changes what your tests expect.
- **Missing HTTP client spans:** call `http.get(...)` on the module object instead of destructuring `get` at file load.
- **MSW dropping headers:** disable `http` with MSW 2 (fixed in MSW 3, [issue #804](https://github.com/mswjs/interceptors/issues/804)).
- **Native ESM missing package spans:** packages loaded as ES modules don't get package spans. HTTP, fetch and manual spans work.
- **Dropped children or interrupted runs:** end spans, close connections and let Jest finish flushing before stopping workers. Work started after file teardown is dropped.
- **Slow teardown:** files wait only for local serialization/write/rename. Check local storage performance. The parent, not every file, pays the network tail.
- **Slow exports or `TEST RESULTS MISSING`:** check the error, token and endpoint access and rerun. A single span exceeding 750 KiB cannot be sent and is reported; reduce its attributes. A 413 below our cap may indicate a smaller intermediary limit.
