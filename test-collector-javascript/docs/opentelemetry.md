# OpenTelemetry (experimental)

Jest, Vitest and Playwright can send results to Test Engine as OpenTelemetry traces.
A **test span** (`test.execution`) records a test result. **Child spans** show the
work underneath: HTTP calls, database queries and your own spans in Jest and
Vitest, or steps in Playwright. For example, in Vitest:

```text
test.execution  "checkout charges the card"
├── vitest.test.runner.test.beforeEach
│   └── POST /seed
└── vitest.test.runner.test.callback
    ├── charge                       (manual span)
    └── GET                          (HTTP client)
        └── GET                      (in-process server)
            └── handler              (manual span)
```

JSON uploads stay the default. OpenTelemetry is opt-in and experimental: options,
environment variables and span shape may change in minor releases. Linux and
macOS are supported. Windows support is not established.

<a id="setup-1"></a>

## Install

All three runners support `@opentelemetry/sdk-trace` 2.9+ and 3.x, with no
collector-imposed upper version limit. Choose one matching package set.

SDK 3 requires Node.js 22.15+ and API 1.10+:

```bash
npm install --save-dev buildkite-test-collector \
  '@opentelemetry/api@^1.10' \
  '@opentelemetry/sdk-trace@^3' \
  '@opentelemetry/context-async-hooks@^3' \
  '@opentelemetry/exporter-trace-otlp-proto@~0.301.0'
```

SDK 2.9+ requires Node.js 18.19+ or 20.6+, or your runner's higher minimum:

```bash
npm install --save-dev buildkite-test-collector \
  '@opentelemetry/api@~1.9.0' \
  '@opentelemetry/sdk-trace@^2.9' \
  '@opentelemetry/context-async-hooks@^2' \
  '@opentelemetry/exporter-trace-otlp-proto@~0.223.0'
```

With Yarn Plug'n'Play, declare these packages in the root workspace.

## Jest

Use Jest 29 or 30 with `jest-circus`.

1. [Install the collector and SDK](#install).
2. Enable the reporter in the root config and the runtime in every project:

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

3. Set `BUILDKITE_ANALYTICS_TOKEN` to your suite token and run `npx jest`.
4. Open the run in your Test Engine suite. Each test, including skips and todos,
   has a test span. Install [instrumentation](#child-spans) to add child spans.

<a id="what-gets-recorded"></a>

Children belong to tests, `beforeEach` and `afterEach`, not file loading,
`beforeAll` or `afterAll`. Jest selects instrumentation declared in `rootDir`
and parent package.json files (the running workspace under PnP).

An application-owned provider keeps its children. Skip its setup when
`JEST_WORKER_ID` is set to send them to Buildkite instead.

With `jest.retryTimes`, each attempt is a separate execution. A fail-then-pass
test still fails the Test Engine run. Each failed default-branch attempt counts
toward auto-quarantine's threshold when enabled. JSON mode sends only the final result.

Keep your usual test environment. A `@jest-environment` docblock does not opt a
file out. For a custom runtime, use
`require('buildkite-test-collector/jest/runtime').extend(MyRuntime)`.
Reporter `token` and `tags` options still work. In TypeScript, use `satisfies Options`
with `Options` from `buildkite-test-collector/jest/reporter`.

### Jest limits and cancellation

- Jest 30.0–30.1 can lose `afterEach` children in files mixing concurrent and sequential tests.
- With API 1.10, native ESM supports HTTP, fetch and manual spans, but not package instrumentation.
- In Jest 29 with jsdom or happy-dom, SDK 3 package instrumentation is skipped with a warning. Test results, HTTP and fetch still record. Use Jest 30, the Node environment or SDK 2 for package spans.
- SDK 3 rejects nested plain-object attributes created inside a test. Use null-prototype maps (`Object.create(null)`).
- `trace.getActiveSpan()` is non-recording: changing or ending it doesn't change the test result.
- On completion or `--bail`, Jest gives uploads up to 10 seconds. Healthy runs finish as soon as uploads do. An already-scheduled exporter retry can keep Node alive for up to 10 more seconds during an outage.
- SIGTERM stops new files from starting and gives uploads up to 5 more seconds. Already-running files can finish, but results arriving after the final upload snapshot are not included.

## Vitest

<a id="requirements"></a>

Use Vitest 4.0.11+ for child spans. Vitest 5 requires Node.js 22.12+.

<a id="setup"></a>

1. [Install the collector and SDK](#install). SDK 3 needs Node.js 22.15+ even with Vitest 4.
2. Enable the reporter and native tracing in the root config:

   ```js
   // vitest.config.js
   import { defineConfig } from 'vitest/config'
   import { createRequire } from 'node:module'
   const require = createRequire(import.meta.url)

   export default defineConfig({
     test: {
       reporters: [
         'default',
         ['buildkite-test-collector/vitest/reporter', { otelEnabled: true }],
       ],
       includeTaskLocation: true,
       experimental: {
         openTelemetry: {
           enabled: true,
           sdkPath: require.resolve('buildkite-test-collector/vitest/otel.js'),
         },
       },
     },
   })
   ```

3. Set `BUILDKITE_ANALYTICS_TOKEN` to your suite token and run `npx vitest run`.
4. Open the run in your Test Engine suite. Add [instrumentation](#child-spans)
   for HTTP, fetch and package calls beneath each test.

Use `require.resolve` for pnpm and Yarn too. In CommonJS, omit `createRequire`.
Projects inherit root tracing unless they override `enabled` or `sdkPath`.
Instrumentation comes from each project's package.json and parents.

<a id="what-you-get"></a>

Retries share one execution with every attempt underneath. Hook phases appear
only around exported children. Collection, suite work and `beforeAll` are
excluded. HTTP/fetch outside tests creates no spans or trace headers, but servers
started in `beforeAll` can trace requests from tests. Avoid the `vitest.` name
prefix for custom spans from the `vitest` tracer: those spans are excluded.

### Using your own tracing SDK

Vitest loads the collector before `setupFiles`. Starting NodeSDK, Sentry or
another SDK there can send its spans to Buildkite instead of your usual backend.
Choose one:

- **Buildkite tracing:** skip application SDK setup when `process.env.VITEST === 'true'`.
- **Application tracing:** use its SDK module as `sdkPath`, or preload it before Vitest. Buildkite sends test results only. Ensure your usual backend receives its spans.

Your `sdkPath` must default-export an object with `shutdown()`. For a preloaded
SDK, keep its flush/shutdown handling and don't initialize it again in `setupFiles`.

### Vitest package instrumentation

`require()` supports package spans. ES module/TypeScript `import` supports
packages such as `pg` and `redis`, but not entry-point patches such as `express`
and `ioredis`. For those, use `createRequire`:

```js
import { createRequire } from 'node:module'
const require = createRequire(import.meta.url)
const express = require('express')
```

For GraphQL, resolve both `graphql` and its subpaths to CommonJS
([Vitest issue #8414](https://github.com/vitest-dev/vitest/issues/8414)):

```js
// Add to vitest.config.js, using require from above
plugins: [{
  name: 'graphql-commonjs',
  enforce: 'pre',
  resolveId: (id) => (/^graphql(\/|$)/.test(id) ? require.resolve(id) : undefined),
}],
```

Mixing GraphQL builds causes `Cannot use GraphQLSchema ... from another module or realm`.
Entry-point instrumentation cannot record packages in `server.deps.inline`,
without a warning. Yarn Plug'n'Play is best-effort: a `conditions` warning means
package tracing is disabled, but test, HTTP and manual spans still work. Upgrade
Yarn or use `nodeLinker: node-modules`. To leave package tracing off explicitly:

```sh
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici yarn vitest run
```

### Using bktec

Remove `--reporter` flags from both bktec commands, and add `'json'` to the
configured reporters:

```sh
export BUILDKITE_TEST_ENGINE_TEST_CMD="npx vitest run {{testExamples}} \
--outputFile {{resultPath}}"
export BUILDKITE_TEST_ENGINE_RETRY_CMD="npx vitest run \
--testNamePattern '{{testNamePattern}}' --outputFile {{resultPath}}"
export BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false
```

The JSON file is for bktec, not collector uploads. Name-filtered retries also
upload excluded tests as skipped.

<a id="limitations"></a>

### Vitest limits and cancellation

- Watch mode disables OpenTelemetry and uploads nothing. Use `vitest run`.
- Browser mode, VM pools (`vmThreads`/`vmForks`, including per-project pools), Vitest 3, custom SDKs and missing native config send test spans only and warn. Use `threads` or `forks` for VM-pool child spans. Vitest 1–2 upload nothing and warn.
- Child spans depend on Vitest's experimental tracing. If it changes, a warning reports missing/unlinked children while test spans still upload.
- Isolation adds SDK startup cost per file. Disabling it also changes test behavior.
- SIGTERM, including Buildkite cancellation, warns that pending results are lost. Vitest exits before uploads drain. Rerun to recover missing results.
- SIGINT (Ctrl-C, also the first [GitHub Actions cancellation signal](https://docs.github.com/en/actions/reference/workflow-cancellation-reference)) loses pending results without a warning.
- End children before workers exit. Children started after their test ends are dropped and counted in a warning.
- **ID collision:** rename either file named in the warning. Traces stay separate, but results are what Vitest reports.

## Playwright

Requires Playwright 1.41.2 or later.

1. [Install the collector and SDK](#install). Playwright needs neither `context-async-hooks` nor Jest's runtime.
2. Enable the reporter:

   ```js
   // playwright.config.js
   module.exports = {
     reporter: [
       ['line'],
       ['buildkite-test-collector/playwright/reporter', { otelEnabled: true }],
     ],
   }
   ```

3. Set `BUILDKITE_ANALYTICS_TOKEN` to your suite token and run `npx playwright test`.
4. Open the run in your Test Engine suite. Each attempt, including retries and
   skips, has a test span with nested steps, hooks, fixtures, assertions and actions.

No fixture or test changes are needed. Durations include steps outside
Playwright's test timer, but not upload latency. Interrupted attempts are skipped.
Expected failures are reported as passed in both modes. Tests expected to be skipped stay skipped; if they do not skip, they are reported as unknown.

Quoted API values and older parenthesized API arguments are replaced with
`[REDACTED]` in step names. User-written `test.step` titles and assertion/error
text are not redacted: keep secrets out of them. Prefer environment tokens over
reporter options. The collector omits its `token` from Playwright JSON reports,
not other credentials or test output. Redaction failures warn but allow uploads.

Reporter `token` and `tags` options work. Per-test `@key:value` tags (Playwright
1.42+) override reporter tags. Child limits apply per file across workers and retries.
For sharded blob reports, enable this reporter only in `merge-reports`, not in
the shards, to avoid duplicate uploads. UI mode uploads every run with a token:
use a separate suite or disable the reporter for local runs you don't want recorded.

<a id="playwright-delivery-and-interruptions"></a>

### Playwright limits and cancellation

- Tracing is reporter-side only: no worker HTTP/database or manual spans, browser application traces, context propagation into workers or in-test annotations.
- Completion drains uploads for up to 10 seconds. Global timeouts allow up to 1 second within that budget for interrupted callbacks, then record open attempts as skipped.
- Ctrl-C interrupts workers and flushes. A second Ctrl-C can stop uploads and warnings.
- The collector's SIGTERM handler allows 5 seconds, including the interrupted-attempt wait, and shortens any existing drain. Configuration/upload failures do not change Playwright's exit code.

<a id="choosing-instrumentation"></a>

## Child spans

For Jest and Vitest, install instrumentation for the calls you want to record.
For SDK 3 / API 1.10+:

```bash
npm install --save-dev '@opentelemetry/instrumentation-http@^0.301.0' \
  '@opentelemetry/instrumentation-undici@^0.35.0'
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici npx jest
```

For SDK 2 / API 1.9:

```bash
npm install --save-dev '@opentelemetry/instrumentation-http@^0.223.0' \
  '@opentelemetry/instrumentation-undici@^0.33.0'
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici npx jest
```

For Vitest, substitute `npx vitest run`. For the broader bundle, use
`@opentelemetry/auto-instrumentations-node@0.83.1` with SDK 3 or
`@opentelemetry/auto-instrumentations-node@~0.81.0` with SDK 2.

Incidental dependencies don't opt you in, but a resolvable auto-instrumentations
bundle selects its whole bundle, except these defaults:

- Logs/messages: `pino`, `winston`, `bunyan`, `aws-sdk`, `kafkajs`, `amqplib`.
- Low-level I/O and metrics: `net`, `dns`, `fs`, `host-metrics`, `runtime-node`.
- `lru-memoizer` and `router`.
- Browser instrumentation: `fetch`, `xml-http-request`, `document-load`, `user-interaction`, `long-task`. Use `undici` for Node fetch.

Opt in with `OTEL_NODE_ENABLED_INSTRUMENTATIONS`. The disabled list always wins.
Incompatible instrumentation warns and is skipped, leaving results and manual
spans enabled. Evaluate other packages against your own operations before relying on them.

For manual spans, end each span yourself:

```js
const { trace } = require('@opentelemetry/api')
test('does work', async () => {
  await trace.getTracer('my-suite').startActiveSpan('work', async (span) => {
    try { await doWork() } finally { span.end() }
  })
})
```

Children can contain URLs, query strings and database statements. Review what
instrumentation records before enabling it.

### Using dd-trace

Don't let dd-trace and OpenTelemetry both trace HTTP/fetch: conflicting headers
can leave server spans missing. Startup warnings cannot detect every overlap.
Disable all three Datadog network integrations:

```sh
DD_TRACE_HTTP_ENABLED=false \
DD_TRACE_FETCH_ENABLED=false \
DD_TRACE_UNDICI_ENABLED=false \
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici \
NODE_OPTIONS='--require dd-trace/init' npx vitest run
```

This leaves manual Datadog spans enabled. If you preload `dd-trace/register.js`,
keep it in `NODE_OPTIONS`.

## Billing

Test spans count as test executions, like JSON uploads, at no extra charge.
This includes each Jest/Playwright retry attempt and Vitest's name-filtered skips.
Children are billed by data ingested and require a plan that includes them.
Playwright records all step categories, so review volume before enabling a large suite.

Test results are always sampled. To reduce children, use
`OTEL_TRACES_SAMPLER=traceidratio` with `OTEL_TRACES_SAMPLER_ARG` from 0 to 1,
or `always_off` to drop all children. In Jest and Playwright, `parentbased_*`
samplers (the default) keep every child. Vitest honors their root sampler and
can reduce children. Children use environment sampling/limits, not a provider's
code-configured sampler.

## Settings

Use a suite API token, or an agent OIDC token granted `write_uploads` by your
suite's OIDC policy. Run keys group results and must be 1–255 printable ASCII
characters without spaces.

| Variable | What it does | Default |
| --- | --- | --- |
| `BUILDKITE_ANALYTICS_TOKEN` | Authenticates uploads. A nonblank reporter `token` takes priority | Not set |
| `BUILDKITE_TESTS_OTLP_TOKEN` | Overrides the token for OpenTelemetry | Suite token |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | HTTP or HTTPS destination | `https://tests-otlp.buildkite.com/v1/traces` |
| `BUILDKITE_ANALYTICS_KEY` | Run key | Derived from CI, or generated locally |
| `BUILDKITE_ANALYTICS_DEBUG_ENABLED` | Prints selected instrumentation and JSON upload debug output | `false` |
| `OTEL_NODE_ENABLED_INSTRUMENTATIONS` | Selects comma-separated short names | Declared packages, minus exclusions above |
| `OTEL_NODE_DISABLED_INSTRUMENTATIONS` | Removes names, even from the enabled list | None |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` | Maximum test spans per upload, at most 1,000 | 1,000 |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` | Maximum child spans per upload | 512 |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` | Children accepted per file, including open spans | 2,048 |
| `OTEL_RESOURCE_ATTRIBUTES` | Resource attributes on every span | None |

Sizes must be positive whole numbers. Invalid values warn and use defaults.
Uploads also have a 750 KiB uncompressed protobuf cap. Large spans reduce batch
counts. Test results ignore `OTEL_SPAN_*` and `OTEL_TRACES_SAMPLER`.
`OTEL_EXPORTER_OTLP_*` and `OTEL_BSP_*` don't control uploads.
For instrumentation diagnostics, install an OpenTelemetry `diag` logger at DEBUG.
`OTEL_LOG_LEVEL=DEBUG` alone doesn't install one.

OTLP ignores `HTTP_PROXY`/`HTTPS_PROXY`, unlike JSON uploads. Use bktec's relay
for a proxy. Your own OTLP endpoint must forward `Authorization`,
`Buildkite-Tests-Run-Key` and `Buildkite-Tests-Span-Stream`.

### bktec's relay

The optional relay requires `bktec run` 3.5.0+ inside Buildkite with agent OIDC:

```sh
export BUILDKITE_TESTS_OTLP_RELAY=true
export BUILDKITE_TESTS_OTLP_RELAY_BATCHING=true
export BUILDKITE_TEST_ENGINE_UPLOAD_RESULTS=false
```

It sets the collector endpoint/token, acknowledges exports locally and retries
delivery in the background. Its transport honors `HTTP_PROXY` and `HTTPS_PROXY`.

<a id="delivery-and-interruptions"></a>

## Uploads and cancellation

Uploads run in the background and retry temporary failures. Test files wait for
local writes, not the network. Runner-specific completion and signal behavior
is described in [Jest](#jest-limits-and-cancellation),
[Vitest](#vitest-limits-and-cancellation) and [Playwright](#playwright-limits-and-cancellation).

- **`TEST RESULTS MISSING`:** fix the earlier warning and rerun the suite.
- **Child upload rejected:** child losses don't block test results. A plan/allowance rejection (HTTP 403) stops child uploads for the rest of the run and warns. Other permanent child failures affect only that batch.
- **SIGKILL or crash:** results not yet uploaded can be lost without a warning. There is no replay tool.

OpenTelemetry never falls back to JSON on configuration or delivery failures.
Fix warnings and rerun, or turn off `otelEnabled` to use JSON.

<a id="troubleshooting-and-known-limits"></a>

## Troubleshooting

- **No spans:** check runner config, SDK versions, token, endpoint and temporary-directory permissions/free space.
- **Missing children:** check your plan, instrumentation warnings, mocked APIs and the runner's provider rules. In Jest's jsdom environment, supply fetch if needed. End spans before teardown and close connections. Still-open spans at worker exit are dropped and counted.
- **Vitest children missing after an upgrade:** follow the warning to find a collector update or report the Vitest version.
- **Changed stacks:** instrumentation/context managers add frames. Use explicit labels if names or snapshots contain stacks.
- **Changed headers/messages:** disable the affected instrumentation with `OTEL_NODE_DISABLED_INSTRUMENTATIONS`.
- **Missing HTTP client spans:** call `http.get(...)` on the module instead of destructuring `get` at file load.
- **Missing HTTP server spans with dd-trace:** follow [Using dd-trace](#using-dd-trace).
- **MSW drops headers:** disable `http` with MSW 2. MSW 3 fixes [issue #804](https://github.com/mswjs/interceptors/issues/804).
- **Mocks swallow uploads:** Jest workers don't upload, but in-band/parent-process mocks can intercept them. With nock's `disableNetConnect`, allow `nock.enableNetConnect(/tests-otlp\.buildkite\.com|127\.0\.0\.1/)`. With MSW, bypass the collector POST. A mock response can hide lost results.
- **Slow teardown:** check local storage performance. Network waits happen in the parent, not per file.
- **Slow exports/missing results:** check token and endpoint access, then rerun. Reduce attributes on spans over 750 KiB. HTTP 413 below that cap can mean an intermediary has a smaller limit.
- **Duplicates:** retries after a lost acknowledgment can duplicate executions until server deduplication.

## Upgrading

Replace direct `@opentelemetry/sdk-trace-base` with `@opentelemetry/sdk-trace`
2.9+. Custom providers need `TracerProvider`, not `BasicTracerProvider`, and
processors take `{ exporter }` options. Collector config is unchanged.

From a 1.13.0 beta, replace the collector's `testEnvironment` with `runtime` and
keep your usual environment. Batch sizes now limit parent uploads, the child
queue size caps children per file, and `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE`
is ignored. See [Settings](#settings) and the [CHANGELOG](https://github.com/buildkite/bktest/blob/main/test-collector-javascript/CHANGELOG.md).
