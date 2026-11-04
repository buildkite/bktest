# OpenTelemetry submission (experimental)

## What it does

```js
// jest.config.js
module.exports = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', { otelEnabled: true }],
  ],
  testEnvironment: 'buildkite-test-collector/jest/environment',
  runtime: 'buildkite-test-collector/jest/runtime', // Optional: record work inside tests
  testLocationInResults: true,
}
```

We send each Jest result to Test Engine as an OpenTelemetry span: a timed operation.
You keep the same test names, locations and history as JSON uploads.
The optional `runtime` setting adds spans for work inside each test, such as HTTP requests and database queries.
These child spans add billed decoded GB, not test executions. This feature is off by default, supports Jest only, and may change before stable.

## Setup

```bash
npm install --save-dev '@opentelemetry/api@~1.9.0' \
  '@opentelemetry/sdk-trace-base@^2.0.0' '@opentelemetry/resources@^2.0.0' \
  '@opentelemetry/exporter-trace-otlp-proto@>=0.200.0 <0.300.0'
# Also install this if you want child spans:
npm install --save-dev '@opentelemetry/context-async-hooks@^2.0.0'
```

- Use Jest 29 or newer with its default `jest-circus` runner and the config above. Both the reporter option and environment are required.
- Use the JavaScript 2.x SDK line: API 1.9, SDK/resources/context 2.x, exporter 0.200–0.2xx. It needs Node.js 18.19 or 20.6 and newer. SDK 3 is not supported; keep these version ranges when installing.
- Set `BUILDKITE_ANALYTICS_TOKEN` to your suite API token, or use an agent OIDC token with `write_uploads`. You can also pass the reporter's `token` option. A nonblank `BUILDKITE_TESTS_OTLP_TOKEN` takes priority.
- If you use bktec's local relay, it sets the endpoint and relay token for you. Relay-only credentials cannot upload JSON fallback results.
- We send to `https://tests-otlp.buildkite.com/v1/traces`. To use another endpoint, set `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` to an HTTP or HTTPS URL with a host.
- If you use a proxy, forward `Buildkite-Tests-Run-Key` and `Buildkite-Tests-Span-Stream` (`test` or `child`). Requests use gzip protobuf over HTTP and `Authorization: Token token="…"`.
- Reporter `tags` still work. Only `true` or the case-insensitive string `'true'` enables `otelEnabled`; `'false'`, `'1'` and `1` do not.
- On Yarn Plug'n'Play, declare `jest-environment-node` at the same version as Jest. Changing the environment or runtime may make the first run slower while Jest rebuilds its cache.

For jsdom, happy-dom or a custom environment, wrap your existing class:

```js
// buildkite-jsdom-environment.js
const { extend } = require('buildkite-test-collector/jest/environment')
module.exports = extend(require('jest-environment-jsdom').default)
// jest.config.js: testEnvironment: './buildkite-jsdom-environment.js'
```

You can keep your environment's setup methods. For TypeScript, type `extend` with your base class:

```ts
import NodeEnvironment from 'jest-environment-node'
const { extend } = require('buildkite-test-collector/jest/environment') as {
  extend: (Base: typeof NodeEnvironment) => typeof NodeEnvironment
}
export default class BuildkiteEnvironment extends extend(NodeEnvironment) {}
// jest.config.js: testEnvironment: './buildkite-environment.ts'
```

A custom runtime can use `require('buildkite-test-collector/jest/runtime').extend(MyRuntime)` too.

## What gets recorded

```js
const { trace } = require('@opentelemetry/api')
test('does work', async () => {
  await trace.getTracer('my-suite').startActiveSpan('work', async (span) => {
    try { await doWork() } finally { span.end() }
  })
})
```

- Every test gets a `test.execution` span, including skipped and todo tests. Retries get one span per attempt, including Jest 30.5's `entireDescribe` retries.
- Tests keep separate traces. Agent v3.110+ with tracing enabled connects them to the job using `TRACEPARENT` and `TRACESTATE`.
- Results include `beforeAll` and `afterEach` failures. Durations use the real clock, not fake timers, and stop when the test finishes rather than after `afterAll`.
- With `runtime` enabled, tests and `beforeEach`/`afterEach` hooks can record manual and selected instrumentation spans. Setup-file hooks, callbacks, generators, concurrent tests, fake timers, `resetModules` and `isolateModules` are supported.
- Child spans can include an in-process server handling the test's request. File loading, `beforeAll`, `afterAll` and unrelated operations are not recorded as children.
- A child started before its file finishes can still end and export before the worker exits. New children after file teardown are dropped and counted; unfinished children are counted at worker exit.
- We leave your own OpenTelemetry provider, context manager and propagation settings alone. While your provider is registered, only test results go to Buildkite. Child recording resumes after `trace.disable()`.
- Test results are always sampled and use random IDs even if you mock `Math.random`. Child spans follow normal SDK sampling and span limits. We never trace the collector's own upload requests.
- Test details include the joined `describe` titles, test title, file, optional line number, result and execution ID. Failures include a reason (up to 1,024 Unicode code points) and exception messages/stacks (up to 10,243).
- We include available suite, organization, CI run, worker, Node.js, branch/tag and commit details. Add tags with the reporter's `tags` option, or extra OpenTelemetry details with `OTEL_RESOURCE_ATTRIBUTES`.

## Choosing instrumentation

```bash
npm install --save-dev '@opentelemetry/instrumentation-http@>=0.200.0 <0.300.0' \
  '@opentelemetry/instrumentation-undici@>=0.11.0 <0.34.0'
OTEL_NODE_ENABLED_INSTRUMENTATIONS=http,undici npx jest
```

We select `@opentelemetry/instrumentation-*` packages declared in your Jest `rootDir`'s package.json or its parents, including workspace roots.
We also select packages supplied by a resolvable `@opentelemetry/auto-instrumentations-node`, including non-hoisted pnpm installs.
An incidental package installed by another dependency, such as `@sentry/node`, does not opt you in.
Use instrumentation for the 2.x SDK line: HTTP 0.200–0.2xx and undici 0.11–0.33; auto-instrumentations-node 0.81 and older use that line.
Other SDK lines warn and are skipped without affecting test results or manual spans.

- `OTEL_NODE_ENABLED_INSTRUMENTATIONS` selects **only** the comma-separated names you list, including default-off names. Leave off the `@opentelemetry/instrumentation-` prefix.
- `OTEL_NODE_DISABLED_INSTRUMENTATIONS` removes names and always wins. For example, `OTEL_NODE_DISABLED_INSTRUMENTATIONS=express,graphql,redis,pg` keeps HTTP/fetch without those package spans.
- We exclude `pino`, `winston`, `bunyan`, `aws-sdk`, `kafkajs` and `amqplib` by default because they can change logs or messages your tests assert on.
- We also exclude `net`, `dns` and `fs` to reduce noise; `host-metrics` and `runtime-node` only collect metrics.
- Browser instrumentations `fetch`, `xml-http-request`, `document-load`, `user-interaction` and `long-task` are off. For Node's `fetch`, use `undici`.
- Set `BUILDKITE_ANALYTICS_DEBUG_ENABLED=true` to see selected names once per worker. Selection stays specific to each project, even when projects share a worker.

Typical counts below exclude the test span. Versions and settings can change these counts.

| Operation | Child spans |
| --- | --- |
| HTTP/HTTPS request | 1 client, plus 1 server if it runs in an instrumented worker |
| Node `fetch` | 1 undici client, plus an in-process HTTP server span |
| Simple Express 4 route | 3 Express spans, plus 2 HTTP spans |
| Express 5 with `router` and `express` | Can overlap; a supertest request can total about 11 |
| One-field async GraphQL query | 4: parse, validate, execute, resolver; more fields can add more |
| Redis connect/SET/GET | 3 |
| pg connect/SELECT | 2; a pool can add a pool-connect span |
| Opt-in `net`/`dns` | 1 per new connection or lookup; one Express request added 2 lookups |
| Manual span | 1 per ended span belonging to a test |

We test HTTP/fetch, Express 4, GraphQL, Redis and pg in CI on Jest 29.7.0 and 30.5.2.
Manual checks also covered operations in Fastify 5, Koa 2, NestJS 11, Apollo Server 5, mysql2 3, MongoDB 6/Mongoose 8, ioredis 5, Knex, Sequelize and TypeORM. These are not full compatibility guarantees.

## Troubleshooting and known limits

- **No spans:** check the reporter and environment settings, installed SDK versions, endpoint and run key; configuration errors warn and use JSON if a JSON token is available.
- **Unsupported Jest:** Jest 28 and older or `jest-jasmine2` use JSON; upgrade to Jest 29+ and `jest-circus`. Beta.1 supported Jest 27/28, but beta.2 onward needs a JSON token on those versions.
- **Wrong environment:** files without the collector environment use JSON while other files export spans; check per-file `@jest-environment` comments or wrap your custom environment with `extend`.
- **No fallback results:** JSON needs reporter `token` or `BUILDKITE_ANALYTICS_TOKEN`; set one if you rely on fallback. With no credential, OpenTelemetry stays off and legacy upload reports a missing token.
- **Invalid run key:** `BUILDKITE_ANALYTICS_KEY` or its CI-derived value must be 1–255 printable ASCII characters without spaces; fix it before rerunning. Use a new process if you need to change the run key after export starts.
- **`TEST RESULTS MISSING`:** an export failed or timed out; check the reported HTTP status/network error and allow the endpoint. Tests still keep their normal exit code. Pending exports may arrive later; dropped test spans mean missing executions.
- **Duplicates:** projects differing only in `testEnvironmentOptions`, or custom runners rewriting config, can make ownership unclear; give projects distinct configurations. We warn and send JSON too rather than risk losing results.
- **Missing children:** look for `OpenTelemetry child span export disabled` or `OpenTelemetry instrumentation skipped`; use compatible packages and avoid mocking the instrumentation's API. A mocked/incomplete API skips package spans for that file, not test results.
- **Your own provider:** setup code, hooks or `NODE_OPTIONS` registering a provider leave test results only; skip your provider setup when `JEST_WORKER_ID` is set to collect children. SDK 2.x cannot add processors later.
- **Dropped children:** warnings count queue overflow, export failure, children started after teardown and children still open at exit; end spans and close connections before the file finishes. Jest clears sandbox timers and can force-exit workers.
- **Changed headers/messages:** HTTP/fetch add `traceparent`, and message instrumentations can add empty fields too; disable affected names with `OTEL_NODE_DISABLED_INSTRUMENTATIONS`. There is no propagation-only switch.
- **Changed stacks:** package instrumentation adds a few collector frames to errors created while modules load, which can change test names or snapshots containing stacks; select only `http,undici` if you need to avoid those extra frames.
- **Destructured HTTP imports:** top-level `const { get } = require('http')` or `import { get } from 'node:http'` can miss client spans/headers; use `http.get(...)` on the module object instead.
- **nock/MSW blocking export:** allow `tests-otlp.buildkite.com` and the local relay's `127.0.0.1`, or bypass them in MSW. Restoring HTTP with `nock.restore()` or `server.close()` across files is supported.
- **MSW headers:** MSW 2.x through 2.15 (`@mswjs/interceptors` <0.42, [issue #804](https://github.com/mswjs/interceptors/issues/804)) can drop caller headers for URL-string requests; disable `http` instrumentation.
- **Native ESM:** `--experimental-vm-modules` supports manual, HTTP and fetch spans, not package instrumentation; use CommonJS for automatic package spans. CommonJS aliases are supported; local mapped mocks are left alone.
- **Browser environments:** jsdom has no fetch; supply your own if needed. Happy-dom's fetch records HTTP spans. These environments support children when wrapped with `extend`.
- **Incoming messages:** gRPC/amqplib can start separate traces, and a checked Socket.IO request/ack produced no children; configure your suite's incoming propagation if needed.
- **Other instrumentation:** upstream packages are not all verified; test your operations before relying on them. `@fastify/otel` and `@prisma/instrumentation` need explicit suite setup; Fastify's standard instrumentation is deprecated in favor of `@fastify/otel`.
- **Names and failure text:** execution-name prefixes/suffixes are not sent; use tags or JSON if you need them. Jest 30.5+ spans omit extra internal frames for non-Error throws; Jest 29–30.4 circular causes can stop JSON results, so upgrade to 30.5+.
- **Late rejections:** Jest 30.5 can attribute test-owned rejections during `afterAll`; Jest 29 treats these as file errors. Compare with the same Jest version's JSON results.
- **Export tuning:** test queue/batch defaults are 8,192/120 with a 1-second delay; children use 2,048/512 and 5 seconds. Set `BUILDKITE_TESTS_OTEL_{TEST,CHILD}_SPAN_{QUEUE,BATCH}_SIZE` using decimal integers 1–2,147,483,647 and batch ≤ queue; invalid values warn and use defaults.
- **Large batches:** the server rejects bodies over 900 KiB; lower the batch size for large failures. Test overflow waits in memory without a cap; child overflow drops children, never results. Each span/event allows 128 attributes and spans allow 100 events; string attributes are capped at 10,243 code points, with long test-name identity preserved.
- **Slow or failed exports:** each request times out after 30 seconds; we retry 429/502/503/504, timeouts and connection errors, not other HTTP errors. Test and child exports have separate 30-second failure budgets per worker; once spent, exports continue without holding up later files. Healthy flushes can take longer.
- **Ignored settings:** `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*`, `OTEL_EVENT_*` and `OTEL_TRACES_SAMPLER` cannot redirect or change test results; use the Buildkite settings above. Normal child sampling/limits still apply. There is no `otelSpanFilter` option or extra hook-phase spans.
- **Interrupted runs:** SIGTERM/SIGINT mid-file can lose that file's results without a missing-results warning; allow Jest to finish and flush before stopping the worker.
