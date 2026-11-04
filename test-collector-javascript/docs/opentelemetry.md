# OpenTelemetry (experimental)

Test Engine can receive your Jest results as OpenTelemetry traces.
Each test becomes a span, and the work inside it (HTTP calls, database queries, your own spans) shows up underneath.

```js
// jest.config.js
module.exports = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', { otelEnabled: true }],
  ],
  testEnvironment: 'node', // Keep your existing environment
  runtime: 'buildkite-test-collector/jest/runtime',
  testLocationInResults: true,
}
```

## Setup

```bash
npm install --save-dev '@opentelemetry/api@~1.9.0' \
  '@opentelemetry/sdk-trace-base@^2.0.0' '@opentelemetry/resources@^2.0.0' \
  '@opentelemetry/exporter-trace-otlp-proto@>=0.200.0 <0.300.0' \
  '@opentelemetry/context-async-hooks@^2.0.0'
```

Use Jest 29+ with `jest-circus` and Node.js 18.19 or 20.6 and newer.
It's experimental and off by default. Child spans add data volume, not test executions.

Use API 1.9 with SDK/resources/context 2.x and exporter 0.200–0.2xx, as shown above.
SDK 3 isn't supported yet; keep the 2.x versions shown.

Reporter `token` and `tags` options still work. Enable export with `otelEnabled: true` or the string `'true'` (case-insensitive).
For TypeScript, check your reporter options with `satisfies Options`, importing `Options` from `buildkite-test-collector/jest/reporter`.

To leave a project out, don't set `runtime`. A `@jest-environment` docblock doesn't opt a file out.
For a custom runtime, use `require('buildkite-test-collector/jest/runtime').extend(MyRuntime)`.

## Settings

Set a suite API token, or use an agent OIDC token with `write_uploads`.
The run key is the ID that groups results from one test run; it must be 1–255 printable ASCII characters without spaces.

| Variable | What it does | Default |
| --- | --- | --- |
| `BUILDKITE_ANALYTICS_TOKEN` | Authenticates uploads; reporter `token` takes priority | Not set |
| `BUILDKITE_TESTS_OTLP_TOKEN` | Overrides the token for OpenTelemetry only | Uses the suite token |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | Sets an HTTP or HTTPS destination | `https://tests-otlp.buildkite.com/v1/traces` |
| `BUILDKITE_ANALYTICS_KEY` | Sets the run key | Derived from CI, or generated locally |
| `BUILDKITE_ANALYTICS_DEBUG_ENABLED` | Prints debug output, including upload payloads and selected instrumentation | `false` |
| `OTEL_NODE_ENABLED_INSTRUMENTATIONS` | Selects only these comma-separated short names | Declared packages, with exclusions below |
| `OTEL_NODE_DISABLED_INSTRUMENTATIONS` | Removes names, even from the enabled list | None |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` / `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` | Sets queue/batch sizes (batch maximum 256); overflow waits in memory | 8,192 / 120 |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` / `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` | Sets queue/batch sizes; overflow drops children | 2,048 / 512 |
| `OTEL_RESOURCE_ATTRIBUTES` | Adds OpenTelemetry details | None |
| `Buildkite-Tests-Run-Key` / `Buildkite-Tests-Span-Stream` (headers) | Proxies must forward these and `Authorization` | Run key / `test` or `child` |

We ignore `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*` and `OTEL_TRACES_SAMPLER` for test results; normal child sampling and limits still apply.
Queue and batch sizes must be decimal integers from 1 to 2,147,483,647 (test batches: 256 maximum) with batch ≤ queue; invalid values warn and use defaults.
An OpenTelemetry `diag` logger at DEBUG receives selected names once per managed file; `OTEL_LOG_LEVEL=DEBUG` alone doesn't install a logger.
If you use bktec's local relay, it sets the endpoint and token for you. Relay-only credentials cannot upload JSON fallback results.

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

- Every test gets a `test.execution` span, including skipped and todo tests; retries get one per attempt.
- Results keep names, locations, failures, tags and CI details; durations use real time and stop when the test finishes.
- Tests have separate traces; the Agent with tracing enabled links them to the job using `TRACEPARENT` and `TRACESTATE`.
- Children belong to tests and `beforeEach`/`afterEach`, not file loading or `beforeAll`/`afterAll`.
- Concurrent tests, callbacks, generators, fake timers and module resets work; results include hook failures.
- We always sample test results, never trace our own uploads, and leave your registered OpenTelemetry setup alone.

## Choosing instrumentation

```bash
npm install --save-dev '@opentelemetry/instrumentation-http@>=0.200.0 <0.300.0' \
  '@opentelemetry/instrumentation-undici@>=0.11.0 <0.34.0'
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici npx jest
```

We select instrumentation declared in your Jest `rootDir`'s package.json or its parents, including workspace roots.
We also select packages supplied by a resolvable `@opentelemetry/auto-instrumentations-node`, including non-hoisted pnpm installs.
Packages installed incidentally by another dependency don't opt you in. Selection stays specific to each project, even in a shared worker.
Under Yarn Plug'n'Play, declare instrumentation in the workspace that runs Jest.
Use the SDK 2.x line: HTTP 0.200–0.2xx, undici 0.11–0.33, or auto-instrumentations-node 0.57 to 0.81.
Incompatible instrumentation warns and is skipped without affecting results or manual spans.

We leave logs/messages (`pino`, `winston`, `bunyan`, `aws-sdk`, `kafkajs`, `amqplib`), low-level I/O (`net`, `dns`, `fs`) and metrics (`host-metrics`, `runtime-node`) off by default.
Browser instrumentation (`fetch`, `xml-http-request`, `document-load`, `user-interaction`, `long-task`) is also off; use `undici` for Node's fetch.
You can opt in by listing short names in `OTEL_NODE_ENABLED_INSTRUMENTATIONS`; the disabled list always wins.
We test HTTP/fetch, Express 4, GraphQL, Redis and pg on Jest 29.7.0 and 30.5.2; test other packages against your own operations before relying on them.

## Troubleshooting and known limits

- **No spans:** check the reporter, runtime, SDK versions, endpoint and earlier warnings, and use Jest 29+ with `jest-circus`.
- **No fallback results:** set reporter `token` or `BUILDKITE_ANALYTICS_TOKEN` so files that can't export spans can upload JSON instead.
- **Missing children:** check instrumentation warnings and mocked APIs, skip your own provider setup under Jest, and supply fetch yourself in jsdom if needed.
- **Duplicates:** give projects distinct configurations beyond `testEnvironmentOptions`, because ambiguous configurations send JSON too rather than risk missing results.
- **Changed stacks:** package instrumentation adds collector frames to errors created while a module loads, and failing tests show context-manager frames whenever runtime is set; use explicit labels if test names or snapshots include stacks.
- **Changed headers or messages:** disable the affected instrumentation using the table above if propagation changes what your tests expect.
- **Missing HTTP client spans:** call `http.get(...)` on the module object instead of destructuring `get` at file load.
- **nock/MSW blocking export:** allow the collector's POST (for example, `nock.enableNetConnect(/tests-otlp\.buildkite\.com|127\.0\.0\.1/)`), or bypass it in MSW; a mock answering it makes results disappear without a warning.
- **MSW dropping headers:** disable `http` with MSW 2 (fixed in MSW 3, [issue #804](https://github.com/mswjs/interceptors/issues/804)).
- **Native ESM missing package spans:** packages loaded as ES modules don't get package spans; HTTP, fetch and manual spans work.
- **Dropped children or interrupted runs:** end spans, close connections and let Jest finish flushing before stopping workers; work started after file teardown is dropped.
- **Slow exports or `TEST RESULTS MISSING`:** check the error and allow the endpoint; the 900 KiB limit is on the compressed body, and a 413 loses that batch, so lower the batch size if the warning names 413.
