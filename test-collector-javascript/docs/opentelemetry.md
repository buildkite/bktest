# OpenTelemetry submission (experimental)

> **This feature is still under development and may change.**

With `otelEnabled: true`, each test becomes a `test.execution` span with
`buildkite.execution.via=otlp`; Buildkite synthesizes its execution server-side
instead of receiving `/v1/uploads` JSON. Resources identify the suite, CI run,
worker and VCS ref; run metadata and configured `tags` live on each test span.
Submission is off by default and currently supports Jest only.

## Jest setup

Jest 29+ with the default `jest-circus` runner needs both the reporter and the
collector's environment:

```js
// jest.config.js
module.exports = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', { otelEnabled: true }],
  ],
  testEnvironment: 'buildkite-test-collector/jest/environment',
  runtime: 'buildkite-test-collector/jest/runtime', // Optional: child spans
  testLocationInResults: true, // Capture test line and column
}
```

Reporter options remain `token`, `tags` and `otelEnabled`; only boolean `true`
or the case-insensitive string `'true'` enables OTel (`'false'`, `'1'` and `1`
do not). Changing `testEnvironment` or `runtime` invalidates transformer cache
keys, so the first run transforms files from cold.

Install the optional peer dependencies explicitly:

```bash
npm install --save-dev '@opentelemetry/api@~1.9.0' \
  '@opentelemetry/sdk-trace-base@^2.0.0' '@opentelemetry/resources@^2.0.0' \
  '@opentelemetry/exporter-trace-otlp-proto@>=0.200.0 <0.300.0'
# Also needed for child spans:
npm install --save-dev '@opentelemetry/context-async-hooks@^2.0.0'
```

Use the JavaScript 2.x SDK line: API 1.9, SDK/resources/context-async-hooks 2.x
and exporter 0.200–0.2xx, requiring Node.js 18.19 or 20.6 and newer.
SDK 3 (API 1.10, resources/sdk-trace 3.x, exporter 0.300) is unsupported;
unversioned installs can mix these lines. Peer ranges set minimums so other
dependencies can install newer APIs without conflicts or downgrades, but an
incompatible export setup warns and falls back to JSON. Packages are loaded
only when OTel is enabled.

### Other environments and runtimes

The default environment extends `jest-environment-node`; Yarn Plug'n'Play
projects must declare `jest-environment-node` at the same version as Jest.
Use `extend` for `jest-environment-jsdom`, `@happy-dom/jest-environment`, or
your own environment:

```js
// buildkite-jsdom-environment.js
const { extend } = require('buildkite-test-collector/jest/environment')
module.exports = extend(require('jest-environment-jsdom').default)
// jest.config.js: testEnvironment: './buildkite-jsdom-environment.js'
```

There are no TypeScript declarations; use `require` and type `extend` with
your base class:

```ts
import NodeEnvironment from 'jest-environment-node'
const { extend } = require('buildkite-test-collector/jest/environment') as {
  extend: (Base: typeof NodeEnvironment) => typeof NodeEnvironment
}
export default class BuildkiteEnvironment extends extend(NodeEnvironment) {
  async setup() {
    await super.setup()
    this.global.answer = 42
  }
}
// jest.config.js: testEnvironment: './buildkite-environment.ts'
```

A custom runtime can also be extended:
`module.exports = require('buildkite-test-collector/jest/runtime').extend(MyRuntime)`.
All three environments above support children; jsdom has no `fetch`, while
happy-dom's `fetch` is recorded as HTTP. Native ESM (`--experimental-vm-modules`)
supports manual, HTTP and `fetch` children, but not package patching.

## What gets recorded

- Each test has its own root trace, including skipped/todo tests; `jest.retryTimes`
  (also Jest 30.5's `entireDescribe`) records one span per attempt.
- A failed `beforeAll` gives every test in its block a failed span with that
  error, matching JSON; `afterEach` failures are also included.
- Durations use the real worker clock, unaffected by fake timers, and end when
  the test finishes, not after `afterAll`.
- Final results include test-owned rejections Jest 30.5 attributes during
  `afterAll`; Jest 29 treats these as file errors, leaving the test passed as
  in JSON.
- Test spans always sample and use random IDs independent of `Math.random`.
  On Agent v3.110+ with tracing enabled, `TRACEPARENT` links them to the job span
  without combining tests into one trace.

### Child spans

Set the collector's `runtime` and install the context manager above to export
manual and selected instrumentation spans beneath each test; no setup file is
needed. Inside tests and `beforeEach`/`afterEach` (including setup-file hooks),
the test span is current:

```js
const { trace } = require('@opentelemetry/api')
test('does work', async () => {
  await trace.getTracer('my-suite').startActiveSpan('work', async (span) => {
    try { await doWork() } finally { span.end() }
  })
})
```

- Module-load tracers work, including setup/application modules, `resetModules`
  (also in `beforeAll`) and `isolateModules`; await, timers/fake timers and
  callbacks retain context, concurrent tests stay separate, and retries retain
  only their own attempt's children. Callback-style tests/hooks and generators work.
- Children include spans continuing the test's trace, such as an in-process
  HTTP server; file-load, `beforeAll`, `afterAll` and independent root spans
  are not exported. There are no phase spans or `otelSpanFilter` option.
- Already-started children can end and export after file teardown but before
  worker exit, retaining their original trace/parent and flushing when they end.
  New descendants after that file's teardown are rejected and counted as
  `started after file teardown`; unfinished children count as
  `still open when the worker exited` in the dropped-children warning.
- Jest clears sandbox timers at teardown and can force-exit workers: end spans
  and close long-lived connections, since work that never ends cannot export.
- A suite-owned provider (in setup, a module, `beforeAll`, a test or a per-test
  hook) is never replaced/reconfigured: its spans go only there, collector
  children pause, and test spans still export. Children resume after
  `trace.disable()`. Process-level setup such as `NODE_OPTIONS=--require
  @opentelemetry/auto-instrumentations-node/register` also leaves test spans only.
  The warning says `OpenTelemetry child spans are not exported because the suite
  registers its own tracer provider; to get them, let the collector provide the
  provider under Jest`. Skip your provider setup when `JEST_WORKER_ID` is set
  to collect children; SDK 2.x cannot add processors after provider construction.
- Suite context managers and propagators are left alone; children use normal
  SDK settings, so `OTEL_TRACES_SAMPLER=traceidratio` can sample them out, never
  test results. Collector requests to Buildkite are never traced.

Children add billed decoded GB, not test executions. Counts vary by operation,
instrumentation version and configuration; examples below exclude the test root:

| Instrumentation | Example children |
| --- | --- |
| `http` | 1 client per HTTP/HTTPS request, plus 1 server in an instrumented worker |
| `undici` | 1 client per `fetch`, plus an in-process `http` server span |
| `net`, `dns` | 1 `tcp.connect` per new connection, 1 per lookup (an Express request added 2 `dns.lookup` spans) |
| `express` | 3 for a simple Express 4 route (query, expressInit, handler), plus 2 HTTP spans |
| `router` | Express 5 overlaps with `express`: a supertest request can add about 11 spans in total |
| `graphql` | 4 for a one-field async query (parse, validate, execute, resolver); more fields can add more |
| `redis` | 3 for connect/SET/GET |
| `pg` | 2 for connect/SELECT; a pool can add a pool-connect span |
| Manual | 1 per ended span in the test's trace |

### Choosing instrumentation

The collector selects `@opentelemetry/instrumentation-*` declared in the Jest
`rootDir`'s package.json or ancestors up to the filesystem root (workspace roots
included), plus dependencies of resolvable `@opentelemetry/auto-instrumentations-node`
(including non-hoisted pnpm packages), not incidental hoisted packages from
dependencies such as `@sentry/node`. It uses upstream definitions/version ranges
for CommonJS packages, including file-load routes and `moduleNameMapper` aliases;
local mapped mocks are not patched.

Use 2.x SDK-line instrumentation, for example HTTP 0.200–0.2xx and undici
0.11–0.33 (auto-instrumentations-node 0.81 and older):

```bash
npm install --save-dev '@opentelemetry/instrumentation-http@>=0.200.0 <0.300.0' \
  '@opentelemetry/instrumentation-undici@>=0.11.0 <0.34.0'
```

Other SDK lines are skipped with `OpenTelemetry instrumentation skipped: …`
once per run; test and manual spans are unaffected.
Selection is on by default except for these groups:

- Observable log/message changes: `pino`, `winston`, `bunyan`, `aws-sdk`, `kafkajs`, `amqplib`.
- Low-level noise: `net`, `dns`; filesystem-call noise: `fs` (also off upstream).
- Metrics only: `host-metrics`, `runtime-node`.
- Browser only: `fetch`, `xml-http-request`, `document-load`, `user-interaction`, `long-task` (Node fetch uses `undici`).

`OTEL_NODE_ENABLED_INSTRUMENTATIONS` selects only its comma-separated names,
including default-off names; `OTEL_NODE_DISABLED_INSTRUMENTATIONS` then removes
names and always wins. Omit the `@opentelemetry/instrumentation-` prefix:
`OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,fs` selects just those two;
`OTEL_NODE_DISABLED_INSTRUMENTATIONS=express,graphql,redis,pg` keeps HTTP/fetch.
These switches never disable test results; there is no extra reporter option
or bridge. `BUILDKITE_ANALYTICS_DEBUG_ENABLED=true` logs the union of selected
names once per worker at exit, including later projects in that worker.

- CI-verified on Jest 29.7.0/30.5.2: Express 4.22.3/instrumentation 0.71.1, GraphQL 16.14.2/0.69.0, redis 5.12.1/0.71.0, pg 8.23.1/0.69.0.
- Checked manually outside CI: default Fastify 5.12.8/0.57.0, Koa 2 routes, NestJS 11 (Express), Apollo Server 5 `executeOperation`, mysql2 3, MongoDB 6/Mongoose 8, ioredis 5, Knex/Sequelize/TypeORM queries; these cover exercised operations, not full feature sets.
- Other upstream instrumentations are not individually verified; Fastify's standard instrumentation is deprecated in favor of `@fastify/otel`.
- Incoming propagation is partial: no sandbox W3C propagator is installed, so gRPC server/amqplib consumer spans can start separate traces and not export as children; the checked Socket.IO incoming request/ack produced no children.
- `@fastify/otel` and `@prisma/instrumentation` are not auto-selected; explicit suite setup worked in manual checks.

## Identity and attributes

Scope, name and location match JSON, preserving test history:

| Span attribute | Value | Execution field |
| --- | --- | --- |
| `buildkite.test.scope` | `describe` titles joined with spaces | scope |
| `buildkite.test.name` | test title | name |
| `test.suite.name` | `describe` titles joined with spaces | — |
| `test.case.name` | scope and name joined with a space | — |
| `code.file.path` | file relative to `rootDir`, with location prefix | file name, location |
| `code.line.number` | test line when `testLocationInResults` is on | location |
| `test.case.result.status` | passed → `pass`, failed → `fail`, pending/skipped/todo → `skipped` | result |
| `buildkite.test.execution.external_id` | generated UUID per span | external ID |

jest-circus omits `ROOT_DESCRIBE_BLOCK` from the whole name path: even a test
with that title inside `describe('ordinary')` has that name and an empty scope.
Failed spans have error status with the failure's first line (1,024 code points)
and one `exception` event per failure containing the message/stack (10,243).
Unicode code points count once, including emoji. Failure text matches JSON except:

- Jest 30.5+ wraps non-Errors (including string timeouts) after the last hook,
  adding about ten internal stack frames to JSON but not spans; reasons and
  test frames match.
- Jest 29–30.4 fails the entire file for circular `cause` chains, so JSON sends
  nothing while the span still reports the failed test.

### Resource and run attributes

| Resource attribute | Value | Execution field |
| --- | --- | --- |
| `service.name`, `service.namespace` | suite slug, organization slug, when available | — |
| `cicd.pipeline.run.id`, `cicd.pipeline.run.url.full` | CI run ID, URL | build ID on Buildkite, URL |
| `cicd.worker.id` | Buildkite Agent ID, when available | — |
| `process.runtime.version` | Node.js version | language version |
| `vcs.ref.head.name`, `vcs.ref.head.revision` | branch/tag, commit SHA | branch, commit |
| `vcs.ref.type` | `branch` or `tag` | — |

The SDK adds `telemetry.sdk.*`; use `OTEL_RESOURCE_ATTRIBUTES` for extra resource identity.

| Test span attribute | Value | Execution field |
| --- | --- | --- |
| `buildkite.execution.via` | `otlp` | — |
| `buildkite.run_key` | Test Engine run key | run key |
| `buildkite.run_url` | `BUILDKITE_ANALYTICS_URL` when different from CI URL | URL |
| `buildkite.build_number`, `buildkite.job_id`, `buildkite.step_id` | build number, job UUID, step UUID | number, job ID, step ID |
| `buildkite.message` | commit message | message |
| `buildkite.collector.name`, `buildkite.collector.version` | `js-buildkite-test-collector`, package version | collector, version |
| `buildkite.location_prefix` | configured path prefix | location prefix |
| `buildkite.test.framework.name`, `buildkite.test.framework.version` | `jest`, Jest version | — |
| `buildkite.tag.<key>` | reporter tags, plus `ci.worker.id` from `BUILDKITE_AGENT_ID` | execution tag `<key>` |

`BUILDKITE_ANALYTICS_EXECUTION_NAME_PREFIX` and
`BUILDKITE_ANALYTICS_EXECUTION_NAME_SUFFIX` are not sent: the test title is
unchanged. Use tags to distinguish runs, or keep `otelEnabled` off if you rely on affixes.

## Export settings

- Destination: `BUILDKITE_ANALYTICS_OTLP_ENDPOINT`, default
  `https://tests-otlp.buildkite.com/v1/traces`; only http/https URLs with a host
  are valid.
- Credentials: nonblank `BUILDKITE_TESTS_OTLP_TOKEN`, then reporter `token` or
  `BUILDKITE_ANALYTICS_TOKEN` (suite API token or agent OIDC with `write_uploads`).
  bktec's relay sets the endpoint/token to its loopback listener/local credential
  and forwards with its own credential.
- Requests: gzip OTLP/HTTP protobuf POSTs with `Authorization: Token token="…"`,
  `Buildkite-Tests-Run-Key` and `Buildkite-Tests-Span-Stream: test` or `child`.
  Forward both `Buildkite-Tests-*` headers through an OTLP proxy.
- `OTEL_EXPORTER_OTLP_*` endpoint/header/compression/certificate/protocol settings
  are ignored: other destinations cannot redirect results or leak their credentials.
  `OTEL_TRACES_SAMPLER`, `OTEL_BSP_*`, `OTEL_SPAN_*`, `OTEL_EVENT_*` cannot change test export.
- Test defaults: queue 8,192, batch 120, delay 1,000 ms, export timeout 30 seconds;
  override with `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` and
  `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE`.
  Overflow waits in memory (no cap), rather than dropping results.
- Child defaults: bounded queue 2,048, batch 512, delay 5,000 ms, timeout 30 seconds;
  override with `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` and
  `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE`.
  Child floods can drop children, never displace results.
- Size overrides must be decimal integers 1–2,147,483,647; invalid values warn
  and use defaults, and batch > queue resets both defaults. The server rejects
  bodies >900 KiB: lower batch size for large failures.
- Attributes/exception messages are limited to 10,243 code points, 128 attributes
  per span/event and 100 events per span; long test names retain JSON identity
  (the server truncates names to 10,240 before deriving the test ID).
- Each file flushes tests first, then children, with separate 30-second failure
  budgets per worker. A healthy endpoint can flush longer; failures and waits
  exceeding the remaining budget consume it. After exhaustion, flushes stop
  waiting but exports continue in the background until worker exit.
- Exports retry 429/502/503/504, timeouts and connection errors with backoff;
  other responses (400/401/403/404/413) fail immediately.
- One worker reports one run: changing run identity after export starts warns
  and keeps the original run key.

## Known limits and troubleshooting

- **Headers and messages:** HTTP/fetch add W3C `traceparent`, visible to exact-header
  assertions/snapshots; `OTEL_NODE_DISABLED_INSTRUMENTATIONS=http,undici` opts out.
  Message instrumentations can similarly change headers/attributes/context
  (Kafka, AMQP, SQS/SNS, Lambda ClientContext), even adding empty `headers: {}`,
  `MessageAttributes: {}` or `MessageAttributeNames: []` without injecting context.
  There is no upstream propagation-only switch: disable those instrumentations.
- **Destructured HTTP:** before the first test/per-test hook,
  `const { get } = require('http')` or `import { get } from 'node:http'` can capture
  an unpatched function, giving no client span/header; use `http.get(...)` on
  the module object (`require('http')` or `import http from 'node:http'`).
- **nock/MSW:** allow the OTLP host (`nock.enableNetConnect('tests-otlp.buildkite.com')`,
  plus `127.0.0.1` for bktec) or bypass it in MSW's unhandled-request policy;
  blanket blocking still active at file end blocks the flush. Reset handlers
  without blocking export. Restoring HTTP functions with `nock.restore()` or
  MSW `server.close()` is supported across files; each request passes through
  at most one active collector wrapper, even beneath retained interceptors.
  MSW 2.x through 2.15 (`@mswjs/interceptors` <0.42,
  [#804](https://github.com/mswjs/interceptors/issues/804)) drops caller headers
  for `http.get/request(urlString, options, cb)` with HTTP instrumentation;
  use `OTEL_NODE_DISABLED_INSTRUMENTATIONS=http`.
- **Missing children:** context-manager/instrumentation incompatibility, suite
  providers and child export errors warn, never trigger JSON fallback; look for
  `OpenTelemetry child span export disabled` or `OpenTelemetry instrumentation skipped`.
  A mocked/incomplete API skips package patches for that file with one warning
  while test spans still export. Project selection applies even in shared workers:
  an earlier project's HTTP/fetch selection does not add spans/headers to opted-out files.
- **Dropped children:** the warning is `OpenTelemetry dropped N child span(s)
  (…); test.execution results are unaffected`, with a total `… so far this run
  (…)` at worker exit. Reasons include `not-exported` (queue full), `export-failure`
  (with last OTLP failure), `started after file teardown`, and `still open when
  the worker exited`. Queue overflow is counted at flush, not immediately;
  HTTP 403 (for example a child-span limit) counts as failure but later batches still send.
- **JSON fallback:** missing/incompatible packages, an invalid endpoint or run
  key, or exporter construction failure warn `OpenTelemetry span export disabled:
  …` then `otelEnabled is set, but OpenTelemetry could not be configured …`.
  Missing/mismatched packages include the supported install command. Run keys
  (`BUILDKITE_ANALYTICS_KEY` or CI-derived) must be 1–255 printable ASCII characters
  without spaces; validation precedes OTel loading. Configuration is checked before workers start.
- **Unsupported Jest:** Jest ≤28 or `jest-jasmine2` uses JSON with a warning;
  upgrade to Jest 29+ and circus. Files with an unextended environment use JSON
  while managed files still export spans; warnings name the environment once.
  Projects differing only in `testEnvironmentOptions` (even repeated explicit
  names/IDs), or custom runners rewriting config, can make file ownership
  ambiguous: the reporter warns and uploads JSON too, preferring duplicates
  over lost results. Check per-file `@jest-environment` docblocks.
- **Credentials on fallback:** JSON requires reporter `token` or
  `BUILDKITE_ANALYTICS_TOKEN`; OTLP-only credentials (including bktec) cannot
  upload fallback results. Warnings end `uploading results as JSON instead` or
  `results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set`.
  Without either token there is no OTel warning; legacy upload prints
  `Missing BUILDKITE_ANALYTICS_TOKEN` and sends nothing, as on a developer machine.
- **Missing results:** OTel errors never fail tests/change Jest's exit code.
  Worker setup errors warn `OpenTelemetry could not be set up in this test worker`
  with that file's fallback outcome; span-start errors warn `TEST RESULTS MISSING`.
  Rejected batches warn `TEST RESULTS MISSING: OpenTelemetry dropped N
  test.execution span(s) (export-failure: N, last OTLP failure: …)` with HTTP
  status/timeout/connection error (for example 403, 404, 503, ECONNREFUSED).
  Spans not reaching the exporter count as `not-exported: N`. Exhausted flush
  budgets warn `TEST RESULTS MISSING: N test.execution span(s) were still exporting
  when the collector stopped waiting for them and may not have reached Buildkite
  Test Engine.` Some may arrive later; each span is counted at most once.
  A dropped test span is a missing Test Engine execution.
- **Signals:** SIGTERM/SIGINT mid-file loses that file's spans without a missing-results
  warning (as JSON loses its results); only earlier flushes can have reported losses.
