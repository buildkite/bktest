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

Use Jest 29 or 30.2+ with `jest-circus`. OpenTelemetry SDK 2 needs Node.js 18.19+ or 20.6+.
Jest 30.0 and 30.1 can drop `afterEach` children in files mixing concurrent and sequential tests.
It's experimental and off by default.
OpenTelemetry options, environment variables and span shape may change in minor releases.
Test spans count as test executions, like JSON uploads, at no extra charge. Child spans are billed by data ingested and need a plan that includes them; without it, we still record test results and warn that child spans were dropped.

Install the versions above. SDK 3 and `@opentelemetry/api` 1.10 aren't supported yet.

Reporter `token` and `tags` options work as usual.
For TypeScript, check your reporter options with `satisfies Options`, importing `Options` from `buildkite-test-collector/jest/reporter`.

Upgrading from a 1.13 beta? Remove `testEnvironment: 'buildkite-test-collector/jest/environment'` and add `runtime`.
To leave a project out, don't set `runtime`. That project prints one "OpenTelemetry didn't record" warning and needs a JSON token for fallback results.
A `@jest-environment` docblock doesn't opt a file out.
For a custom runtime, use `require('buildkite-test-collector/jest/runtime').extend(MyRuntime)`.

If you use nock's `disableNetConnect`, allow the collector's POST:

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
| `BUILDKITE_ANALYTICS_DEBUG_ENABLED` | Prints debug output, including upload payloads and selected instrumentation | `false` |
| `OTEL_NODE_ENABLED_INSTRUMENTATIONS` | Selects only these comma-separated short names | Declared packages, with exclusions below |
| `OTEL_NODE_DISABLED_INSTRUMENTATIONS` | Removes names, even from the enabled list | None |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` and `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` | Sets queue and batch sizes (batch maximum 200). Overflow waits in memory | 8,192 queued, 120 per batch |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` and `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` | Sets queue and batch sizes. Overflow drops children | 2,048 queued, 512 per batch |
| `OTEL_RESOURCE_ATTRIBUTES` | Adds resource attributes to every span | None |

OTLP export doesn't use `HTTP_PROXY` or `HTTPS_PROXY`. JSON uploads honour them.
If you route OTLP through your own endpoint, forward `Authorization`, `Buildkite-Tests-Run-Key` and `Buildkite-Tests-Span-Stream`.
We ignore `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*` and `OTEL_TRACES_SAMPLER` for test results. Normal child sampling and limits still apply.
Sizes must be whole numbers, with batch no larger than queue (at most 200 for test spans). Invalid values print a warning and use the defaults.
An OpenTelemetry `diag` logger at DEBUG receives selected names once per test file. `OTEL_LOG_LEVEL=DEBUG` alone doesn't install a logger.
If you use bktec's local relay, it sets the endpoint and token for you. The relay's token can't upload JSON, so also set `BUILDKITE_ANALYTICS_TOKEN` for the fallback.

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

- Every test gets a `test.execution` span, including skipped and todo tests. Retries get one span and count as one test execution per attempt.
- Results keep names, locations, failures, tags and CI details. Durations use real time and stop when the test finishes.
- Tests have separate traces. The Agent with tracing enabled links them to the job using `TRACEPARENT` and `TRACESTATE`.
- Children belong to tests, `beforeEach` and `afterEach`, not file loading, `beforeAll` or `afterAll`.
- Concurrent tests, callbacks, generators, fake timers and module resets work. Results include hook failures.
- We always sample test results, never trace our own uploads, and leave your registered OpenTelemetry setup alone.

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
Packages installed incidentally by another dependency don't opt you in.
Use the SDK 2.x line: HTTP 0.200–0.2xx, undici 0.11–0.33, or auto-instrumentations-node 0.57 to 0.81.
Incompatible instrumentation warns and is skipped without affecting results or manual spans.

We leave logs and messages (`pino`, `winston`, `bunyan`, `aws-sdk`, `kafkajs`, `amqplib`), low-level I/O (`net`, `dns`, `fs`) and metrics (`host-metrics`, `runtime-node`) off by default.
Browser instrumentation (`fetch`, `xml-http-request`, `document-load`, `user-interaction`, `long-task`) is also off. Use `undici` for Node's fetch.
You can opt in by listing short names in `OTEL_NODE_ENABLED_INSTRUMENTATIONS`. The disabled list always wins.
We test HTTP, fetch, Express 4, GraphQL, Redis and pg on Jest 29.7.0 and 30.5.2. Test other packages against your own operations before relying on them.

## Troubleshooting and known limits

- **No spans:** check the reporter, runtime, SDK versions, endpoint and earlier warnings, and use Jest 29 or 30.2+ with `jest-circus`.
- **No fallback results:** set reporter `token` or `BUILDKITE_ANALYTICS_TOKEN` so files that can't export spans can upload JSON instead.
- **Missing children:** check your plan includes child spans. Check instrumentation warnings and mocked APIs, skip your own provider setup under Jest, and supply fetch yourself in jsdom if needed.
- **Duplicates:** projects whose configs differ only in `testEnvironmentOptions` also upload JSON. Make them differ another way, such as `displayName`.
- **Changed stacks:** package instrumentation adds collector frames to errors created while a module loads. Failing tests show context-manager frames whenever runtime is set. Use explicit labels if test names or snapshots include stacks.
- **Changed headers or messages:** disable the affected instrumentation using the table above if propagation changes what your tests expect.
- **Missing HTTP client spans:** call `http.get(...)` on the module object instead of destructuring `get` at file load.
- **MSW dropping headers:** disable `http` with MSW 2 (fixed in MSW 3, [issue #804](https://github.com/mswjs/interceptors/issues/804)).
- **Native ESM missing package spans:** packages loaded as ES modules don't get package spans. HTTP, fetch and manual spans work.
- **Dropped children or interrupted runs:** end spans, close connections and let Jest finish flushing before stopping workers. Work started after file teardown is dropped.
- **Slow teardown:** each test file waits for one export round trip at teardown. If the endpoint hangs, an in-band run can take about 60 seconds longer.
- **Slow exports or `TEST RESULTS MISSING`:** check the error and allow the endpoint. If the warning names 413, a batch was over 900 KiB compressed and was lost; lower `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE`.
