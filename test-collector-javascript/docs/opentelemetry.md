# OpenTelemetry submission (experimental)

> **This feature is still under development and may change.**

When `otelEnabled` is true, spans are the only submission path. Every test
becomes a `test.execution` span with `buildkite.execution.via=otlp`, which makes
the span itself the submission: Buildkite synthesizes the execution from it
server-side, and nothing is sent to `/v1/uploads`. Resources identify the suite,
CI run, worker and VCS ref that produced the telemetry. Test Engine run metadata
and any `tags` you configure live on each `test.execution` span.

OpenTelemetry submission is off by default. Jest is the only framework that
supports it so far.

## Jest

Jest runs tests in worker processes and reporters in the main process, so the
collector needs both halves: the reporter decides whether the run uses
OpenTelemetry, and a test environment in each worker starts and ends a span
around every test and exports from the worker.

```js
// jest.config.js
module.exports = {
  reporters: [
    'default',
    ['buildkite-test-collector/jest/reporter', { otelEnabled: true }],
  ],
  testEnvironment: 'buildkite-test-collector/jest/environment',

  // Enable column + line capture for Test Engine
  testLocationInResults: true,
}
```

Both settings are required. With `otelEnabled: true` but no `testEnvironment`
(or one that is not the collector's and was not built with its `extend`), the
reporter warns `otelEnabled is set, but test environment "…" is not
buildkite-test-collector/jest/environment …` once per environment and uploads
those files' results as JSON.

`buildkite-test-collector/jest/environment` extends `jest-environment-node`.
With Yarn Plug'n'Play, add `jest-environment-node` (the same version as
`jest`) to your devDependencies so the collector can load it. If your suite
needs another environment, build one with `extend`:

```js
// buildkite-jsdom-environment.js
const { extend } = require('buildkite-test-collector/jest/environment')
module.exports = extend(require('jest-environment-jsdom').default)
```

```js
// jest.config.js
testEnvironment: './buildkite-jsdom-environment.js',
```

The package ships no TypeScript declarations, so a TypeScript environment
loads the module with `require` and gives `extend` the base class's type:

```ts
// buildkite-environment.ts
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
```

```js
// jest.config.js
testEnvironment: './buildkite-environment.ts',
```

OpenTelemetry submission supports Jest 27 and newer.

The reporter options (`token`, `tags`, `otelEnabled`) are the same as before.
`otelEnabled` is on only for `true` and the case-insensitive string `'true'`;
`'false'`, `'1'` and `1` are off, so a value passed through from a CLI or the
environment, for example `otelEnabled: process.env.BUILDKITE_OTEL`, only turns
OpenTelemetry on when it says `true`.

Export needs the OpenTelemetry packages, which are optional peer dependencies
of `buildkite-test-collector` and are not installed with it:

```bash
npm install --save-dev '@opentelemetry/api@~1.9.0' \
  '@opentelemetry/sdk-trace-base@^2.0.0' '@opentelemetry/resources@^2.0.0' \
  '@opentelemetry/exporter-trace-otlp-proto@>=0.200.0 <0.300.0'
```

The collector supports the OpenTelemetry JavaScript 2.x SDK line:
`@opentelemetry/api` 1.9, `@opentelemetry/sdk-trace-base` and
`@opentelemetry/resources` 2.x, and `@opentelemetry/exporter-trace-otlp-proto`
0.200 to 0.2xx. The 3.0 SDK line (`@opentelemetry/api` 1.10,
`@opentelemetry/resources` 3.x, `@opentelemetry/sdk-trace` 3.x and the
0.300 exporter) is not supported yet. Keep the versions in the command above:
without them npm installs the newest packages, which mix the two lines and fail
to resolve. Those SDK versions need Node.js 18.19 or 20.6 and newer. The
packages are only loaded once a run has decided to use OpenTelemetry; with
`otelEnabled` off nothing changes.

### What a run looks like

Each test that Jest runs gets one `test.execution` span, which is a trace of its
own: the span is never nested under anything else. A test retried with
`jest.retryTimes`, including Jest 30.5's `entireDescribe` mode, gets one span per
attempt. A test skipped with `test.skip`,
`xit` or `test.todo` gets a `skipped` span. When a `beforeAll` hook fails, every
test in its block gets a failed span carrying the hook's error, which is also
what the JSON upload reports.

The span's start and end are read from the real clock in the worker, outside
the test's sandbox, so `jest.useFakeTimers()` does not affect the execution's
duration. The final attempt is recorded once the file's results have settled,
using the time the test finished, not the end of `afterAll`. This includes late
test-owned promise rejections that Jest 30.5 attaches to a test during
`afterAll`, as well as `afterEach` failures. Jest 27 and 29 treat those late
rejections as file errors instead; the test's span stays passed, matching the
JSON upload.

Spans are exported with explicit settings on a provider the collector owns:
the sampler always samples, span and trace IDs come from `crypto.randomBytes`
(so a suite that stubs `Math.random` cannot produce colliding IDs), and the
attribute and event limits below apply whatever `OTEL_SPAN_*` is set to.

On Buildkite Agent v3.110 or newer with tracing enabled, the test span links to
the Agent's job span through `TRACEPARENT`, letting you navigate between them
without combining every test into one trace.

### Instrumentation and manual child spans

Starting with 1.13.0-beta.2, add a setup file to connect the tests' OTel API to
the worker's test spans. Without this extra setup, export remains test spans
only, as in beta.1. Install the optional async context manager:

```sh
npm install --save-dev '@opentelemetry/context-async-hooks@^2.0.0'
```

```js
// jest.config.js (in addition to the reporter and environment above)
setupFilesAfterEnv: ['<rootDir>/otel.setup.js'],
```

For manual spans only, `otel.setup.js` can contain just:

```js
require('buildkite-test-collector/jest/otel')
```

The collector waits until the first test starts before configuring children,
so a provider installed by setup files or `beforeAll` wins. It registers a
child provider only if the sandbox's global provider is still unconfigured,
and an async context manager only if no working one is installed. The private
always-on provider still exports `test.execution` independently.

Inside a test or a `beforeEach`/`afterEach` hook, the test span is current:

```js
const { trace } = require('@opentelemetry/api')
test('does work', async () => {
  await trace.getTracer('my-suite').startActiveSpan('work', async (span) => {
    try { await doWork() } finally { span.end() }
  })
})
```

Async work inherits that context across `await`, timers and callback APIs.
Concurrent tests have separate contexts. Test functions are wrapped before
Circus launches them, not at `test_start` (which is too late for concurrent
tests on older Jest versions). Callback-style tests and hooks retain their
arity. `beforeAll`, `afterAll`, file loading and unrelated root traces are not
associated with a test. End child spans before the file finishes; teardown
makes forwarding inert before Jest destroys the sandbox.

For automatic instrumentation, install and explicitly configure the packages
you want. For example, versions verified with SDK 2.x:

```sh
npm install --save-dev '@opentelemetry/instrumentation-http@0.223.0' \
  '@opentelemetry/instrumentation-undici@0.33.0'
```

```js
// otel.setup.js
const { configure, instrumentModule } = require('buildkite-test-collector/jest/otel')
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http')
const { UndiciInstrumentation } = require('@opentelemetry/instrumentation-undici')
const http = new HttpInstrumentation({ enabled: false })
const undici = new UndiciInstrumentation({ enabled: false })
configure({ instrumentations: [http, undici] })
instrumentModule(http, 'http', require('http'), process.versions.node)
instrumentModule(http, 'https', require('https'), process.versions.node)
```

`configure` enables those instrumentation instances on the collector-managed
provider. It does not discover installed npm packages. Unlike Ruby's gem
registry, JavaScript has no registry of installed instrumentation instances.
HTTP core modules are shared with Node; undici (including Node's built-in
`fetch`) uses Node diagnostics channels. Both work inside real Jest workers.
Jest 27 does not expose Node's `fetch` global in its sandbox. For fetch on that
version, extend the collector environment and set
`this.global.fetch = globalThis.fetch` in its constructor after `super(...)`.
The compatibility tests use that subclass with Node's real fetch, not a mock.

**Userland modules need an explicit patch.** Jest's module loader bypasses
the `require-in-the-middle` hooks used by most OTel instrumentation. Merely
installing or enabling a PostgreSQL, Redis or GraphQL instrumentation package
does not patch the copy loaded by Jest. `instrumentModule` schedules a patch of
the actual exports you supply, checks the instrumentation's supported version
range, and unpatches at file teardown. It uses the upstream public but
**experimental** `getModuleDefinitions()` API, including file definitions.
For example, GraphQL 16 with instrumentation 0.69.0:

```js
const { GraphQLInstrumentation } = require('@opentelemetry/instrumentation-graphql')
const graphql = new GraphQLInstrumentation({ enabled: false })
// Include graphql in the configure({ instrumentations: [...] }) call above.
const version = require('graphql/package.json').version
instrumentModule(graphql, 'graphql/execution/execute.js', require('graphql/execution/execute.js'), version)
instrumentModule(graphql, 'graphql/language/parser.js', require('graphql/language/parser.js'), version)
instrumentModule(graphql, 'graphql/validation/validate.js', require('graphql/validation/validate.js'), version)
```

This path is tested with a GraphQL resolver doing real HTTP I/O, not a mocked
resolver or a mock instrumentation package. It does not promise compatibility
with every instrumentation: some patch different files, capture exports before
setup, or need their own lifecycle. Pass the correct module/file definitions
for the version you install. `jest.resetModules()` or `jest.isolateModules()`
creates new userland module instances; the patch applies only to the supplied
instance, not future copies. Native ESM instrumentation is not verified.

### Suite-owned providers (SDK 2.x)

The collector never replaces or reconfigures a suite-owned provider. SDK 2.x
has no public runtime `addSpanProcessor`, so install the forwarder when you
construct your provider, in the sandbox setup file:

```js
const { trace, context } = require('@opentelemetry/api')
const { BasicTracerProvider } = require('@opentelemetry/sdk-trace-base')
const { AsyncLocalStorageContextManager } = require('@opentelemetry/context-async-hooks')
const { createBuildkiteSpanProcessor } = require('buildkite-test-collector/jest/otel')
const provider = new BasicTracerProvider({
  // Keep your sampler, resource, limits and existing processors here.
  spanProcessors: [myProcessor, createBuildkiteSpanProcessor()],
})
trace.setGlobalTracerProvider(provider)
context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable())
```

Your provider's sampler applies to children; sampling them all out does not
sample out test results. Child spans retain your provider's resource and go
to your existing exporters as well as Buildkite. You own your provider's and
instrumentation's lifecycle: the collector does not enable, redirect, flush or
shut them down. Explicit `instrumentModule` calls still patch only the exports
you requested. A provider without the forwarder warns and remains untouched;
test spans still export. Configure the provider in the test sandbox, not only
in a separate Node preload realm.

Phase spans are not implemented in this beta. Children nest directly under
the test span, or under their instrumentation's own parent spans. There is no
`otelSpanFilter` option in JavaScript.

### Identity

A test's scope, name and location are derived exactly as the JSON upload
derives them, so switching `otelEnabled` on keeps each test's history:

| Span attribute | Value | Execution field |
| --- | --- | --- |
| `buildkite.test.scope` | the `describe` titles joined with spaces | scope |
| `buildkite.test.name` | the test title | name |
| `test.suite.name` | the `describe` titles joined with spaces | — |
| `test.case.name` | scope and name joined with a space | — |
| `code.file.path` | the test file, relative to `rootDir`, with any location prefix | file name, location |
| `code.line.number` | the test's line when `testLocationInResults` is on | location |
| `test.case.result.status` | `pass`, `fail` or `skipped` | result |
| `buildkite.test.execution.external_id` | a UUID the collector generates per span | external ID |

Jest results map to `test.case.result.status` as `passed` → `pass`,
`failed` → `fail`, and `pending`, `skipped` and `todo` → `skipped`.

Names follow jest-circus, which leaves its internal `ROOT_DESCRIBE_BLOCK`
marker out of the whole name path, so a test titled `ROOT_DESCRIBE_BLOCK`
inside `describe('ordinary')` is named `ROOT_DESCRIBE_BLOCK` with an empty
scope in both uploads.

A failed test sets the span's status to error with the failure's first line as
its description, cut at 1,024 characters as the server cuts the JSON upload's.
Each failure is an `exception` event whose `exception.message` carries the
rest of Jest's failure message, including the stack trace, truncated to 10,243
characters. Lengths count Unicode code points, as the server and the other
collectors do, so an emoji counts once and is never cut in half.

Failure text matches the JSON upload with two exceptions. On Jest 30.5 and
later, a test that throws something other than an `Error` (`throw 'oops'`, and
every test timeout, which Jest 30.5 throws as a string) has its value wrapped
in a new `Error` by Jest after the test's last hook, so the JSON upload's
message carries about ten Jest-internal stack frames that the span's message
does not; the failure reason and the test's own frames are the same. On Jest
29 to 30.4, an error whose `cause` chain is circular makes Jest fail the whole
file, so the JSON upload sends nothing for it, while the span still reports
the test as failed.

### Resource and run attributes

The `buildkite.execution.via=otlp` marker opts every exported test span into
execution synthesis. Buildkite-specific execution fields are flat
(`buildkite.run_key`, `buildkite.job_id`, ...); producer identity follows
OpenTelemetry resource semantic conventions.

Resources identify entities that apply to every span the worker emits:

| Resource attribute | Value | Execution field |
| --- | --- | --- |
| `service.name` | the Test Engine suite slug, when available | — |
| `service.namespace` | the Buildkite organization slug, when available | — |
| `cicd.pipeline.run.id` | the CI pipeline run ID | build ID on Buildkite |
| `cicd.pipeline.run.url.full` | the CI pipeline run URL | URL |
| `cicd.worker.id` | the Buildkite Agent ID, when available | — |
| `process.runtime.version` | the Node.js version | language version |
| `vcs.ref.head.name` | the branch (or tag) name | branch |
| `vcs.ref.head.revision` | the commit SHA | commit |
| `vcs.ref.type` | `branch` or `tag` | — |

The SDK's default resource also contributes `telemetry.sdk.*` attributes, and
`OTEL_RESOURCE_ATTRIBUTES` remains the standard way to add resource identity.

Each test span also carries the run metadata:

| Span attribute | Value | Execution field |
| --- | --- | --- |
| `buildkite.execution.via` | `otlp` | — |
| `buildkite.run_key` | the Test Engine run key | run key |
| `buildkite.run_url` | a `BUILDKITE_ANALYTICS_URL` that differs from the CI run URL | URL |
| `buildkite.build_number` | the build number | number |
| `buildkite.job_id` | the job's UUID | job ID |
| `buildkite.step_id` | the step's UUID | step ID |
| `buildkite.message` | the commit message | message |
| `buildkite.collector.name` | `js-buildkite-test-collector` | collector |
| `buildkite.collector.version` | this package's version | version |
| `buildkite.location_prefix` | the prefix prepended to test file paths, when configured | location prefix |
| `buildkite.test.framework.name` | `jest` | — |
| `buildkite.test.framework.version` | the Jest version | — |
| `buildkite.tag.<key>` | each reporter `tags` entry, plus `ci.worker.id` from `BUILDKITE_AGENT_ID` | execution tag `<key>` |

The execution name affixes (`BUILDKITE_ANALYTICS_EXECUTION_NAME_PREFIX` and
`BUILDKITE_ANALYTICS_EXECUTION_NAME_SUFFIX`) are not sent over OpenTelemetry;
`buildkite.test.name` is the test title as written. Use `tags` to distinguish
runs that share a suite, or keep `otelEnabled` off if you rely on them.

## What gets sent

The collector exports to `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` (default
`https://tests-otlp.buildkite.com/v1/traces`; it must be an http or https URL
with a host, or the run warns and falls back) and authenticates with
`BUILDKITE_TESTS_OTLP_TOKEN` or, when that is unset or blank, the reporter's
`token` option or `BUILDKITE_ANALYTICS_TOKEN`: either the suite API token or an
agent OIDC token with the `write_uploads` scope. bktec's OTLP relay sets both
`BUILDKITE_*` variables to its loopback listener and local credential, then
forwards spans to Buildkite with its own credential.

Every request is an OTLP/HTTP protobuf `POST` with gzip compression and the
headers `Authorization: Token token="…"`, `Buildkite-Tests-Run-Key` (the run
key) and `Buildkite-Tests-Span-Stream: test` for test spans, or `child` for child
spans. Separate exporters and queues keep children out of test batches. If you relay collector traffic
through your own OTLP proxy, forward both `Buildkite-Tests-*` headers.

The standard `OTEL_EXPORTER_OTLP_*` endpoint, header, compression, certificate
and protocol variables are never read, so credentials the process configures
for another OpenTelemetry destination are not sent to Buildkite, and that
destination's settings do not redirect test results. `OTEL_TRACES_SAMPLER`,
`OTEL_BSP_*`, `OTEL_SPAN_*` and `OTEL_EVENT_*` cannot change test-span export.
The child provider uses normal SDK sampling, including `OTEL_TRACES_SAMPLER`.

Without either token, nothing is exported and the run uses the JSON path as it
always has. There is no OTel warning, but the legacy uploader still prints
`Missing BUILDKITE_ANALYTICS_TOKEN` and uploads nothing, so a suite that
hard-codes `otelEnabled: true` behaves on a developer machine exactly as it
did before.

Each worker exports its spans in batches of up to 120, with a queue of 8,192
spans, a 1,000 ms schedule delay and a 30-second export timeout. Set
`BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` or
`BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` to override the batch or queue size.
Overrides must be positive decimal integers no greater than 2,147,483,647;
invalid values warn and use the corresponding default, and a batch size larger
than the queue size warns and resets both to their defaults. The server rejects
request bodies over 900 KiB, so lower the batch size rather than raise it if a
suite produces very large failure output.

A full queue never drops a result. A file of fast tests finishes spans faster
than the worker can export them, so spans that would overflow the queue are
held in memory and handed to the exporter as earlier batches complete, with
memory as the only cap; the queue size only bounds what is in flight.

Children use a separate bounded queue of 2,048 spans, batches of up to 512,
a 5,000 ms schedule delay and a 30-second export timeout. Set
`BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` and
`BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` to override these sizes, with the
same validation as test sizes. A child flood can drop children but never
displace test results. Both paths share the worker's flush budget, test spans
first. Dropped children warn that `test.execution results are unaffected`;
SDK 2.x does not expose immediate queue-overflow callbacks, so accounting is
reported at flush, not the moment a queue fills.

Span attribute values are truncated to 10,243 characters, each span keeps at
most 128 attributes and 100 events, and each event keeps at most 128
attributes. The attribute limit matches the server, which truncates test names
to 10,240 characters before deriving the test ID, so a long test name keeps
the same identity and history as the JSON upload. Exception messages are cut
at the same 10,243 characters, as the other collectors cut theirs. Both limits
count Unicode code points.

The worker flushes its queue at the end of every test file, because Jest
exits workers without any later hook. A flush has no overall time limit while
the endpoint keeps accepting spans, so a large file against a slow endpoint
can take more than 30 seconds to flush. Flushes in one worker share a
30-second budget that failures consume, as does a single wait that outlasts
what is left of it: a healthy endpoint never consumes it, but once it is used
up, later flushes stop waiting and export continues in the background until
the worker exits. A flush that stops waiting warns about the spans still
exporting at that moment (see below), so that warning is printed even when
Jest later force-exits the worker. The exporter retries on 429, 502, 503,
504, timeouts and connection errors with backoff; other responses (400, 401,
403, 404, 413) fail the batch immediately.

## When something goes wrong

Missing child dependencies or child setup/patch/export failures warn and leave
test-span export enabled. They never select JSON fallback. Check for
`OpenTelemetry child span export disabled` if instrumentation is absent.

The run key (`BUILDKITE_ANALYTICS_KEY`, or the key derived from your CI's
variables) must be 1–255 printable ASCII characters without spaces. An invalid
key disables OpenTelemetry export with a warning before any OpenTelemetry code
is loaded, and the run falls back to the JSON upload. That upload still needs
`BUILDKITE_ANALYTICS_TOKEN`; when the only credential was
`BUILDKITE_TESTS_OTLP_TOKEN` (as with bktec's relay), the warning says that
results will not be uploaded instead.

If the OpenTelemetry packages are missing or come from releases that cannot
export together, the exporter cannot be built, the run key is invalid or
`BUILDKITE_ANALYTICS_OTLP_ENDPOINT` is not an http or https URL, the reporter
warns `OpenTelemetry span export disabled: …` followed by
`otelEnabled is set, but OpenTelemetry could not be configured …` and the
whole run uploads JSON, exactly as it does with `otelEnabled` off. That
decision is made once in the main process before any worker starts. For a
missing package the warning names the module, and for mismatched packages the
error a test span would have hit; both give the `npm install` command, with
the supported version ranges, that installs all four.

Export is decided per test file. Test files that run under a
`testEnvironment` other than the collector's (or one built with its `extend`)
cannot export spans: the reporter warns once per environment and uploads those
files' results as JSON, while files under the collector's environment in the
same run still export spans. The worker that runs a file records, in a
temporary directory the reporter creates for the run, that it is exporting
the file, and the reporter uploads JSON only for files without that record, so
a test is sent over one path. The exception is two projects with the same
configuration apart from `testEnvironmentOptions` (Jest accepts a repeated
explicit project `name` or `id`): if only one of them exported a file, the
reporter cannot tell which, so it warns and uploads both projects' results for
that file as JSON as well, and some appear twice rather than any going
missing. A custom test runner that rewrites the project config before the
environment sees it can make the worker's record miss the reporter's lookup
in the same way, so that file's tests would be sent twice, never lost. Check
the warning if a project uses per-file `@jest-environment` docblocks. The whole run falls back under the legacy `jest-jasmine2` test
runner, which does not send test events to the environment; export needs the
default `jest-circus` runner. Every fallback warning ends with what happens to
the results: `uploading results as JSON instead`, or, when only
`BUILDKITE_TESTS_OTLP_TOKEN` is set, `results will not be uploaded because
BUILDKITE_ANALYTICS_TOKEN is not set`.

OpenTelemetry errors never fail a test or change Jest's exit code. If a worker
cannot set up export, the environment still constructs, the file's tests still
run, and the worker warns `OpenTelemetry could not be set up in this test
worker` with what happens to that file's results instead (normally `uploading
results as JSON instead`). If a span cannot be started, the collector warns
`TEST RESULTS MISSING` for the affected tests and the run continues. When
the endpoint rejects a batch, the end-of-file flush warns
`TEST RESULTS MISSING: OpenTelemetry dropped N test.execution span(s) (export-failure: N, last OTLP failure: …)`,
naming the HTTP status, `timeout`, or the connection error (for example `403`
when OTLP ingest is not enabled for the organization, `404` for a wrong
endpoint, `503` once retries run out, or `ECONNREFUSED`). Spans that never
reached the exporter are counted as `not-exported: N`. When a flush stops
waiting because the 30-second budget is used up, it warns
`TEST RESULTS MISSING: N test.execution span(s) were still exporting when the collector stopped waiting for them and may not have reached Buildkite Test Engine.`
Export continues in the background, so some of those may still arrive. Each
span is counted in one warning at most. Because `test.execution` spans are the
submission, a dropped span is a test execution Test Engine never sees.

Suites that block outgoing HTTP in tests need to allow the endpoint. `nock` and
`msw` patch Node's `http` module, which the worker's exporter shares with the
test sandbox, so a blanket `nock.disableNetConnect()` or
`onUnhandledRequest: 'error'` that is still active when the test file finishes
blocks the flush. Allow the host with
`nock.enableNetConnect('tests-otlp.buildkite.com')` (plus `127.0.0.1` when
using bktec's relay) or restore the interceptors in `afterAll`.

One worker process reports one run. Run identity is fixed when export starts in
a worker: reconfiguring with a different run key warns and keeps attributing
results to the original run.

## Limitations

A worker killed by SIGTERM or SIGINT in the middle of a test file loses that
file's spans without a `TEST RESULTS MISSING` warning. Node has no exit event
when a signal ends the process, and the collector does not install signal
handlers in Jest's workers, so only spans counted by an earlier end-of-file
flush are warned about. The JSON upload loses the same results.
