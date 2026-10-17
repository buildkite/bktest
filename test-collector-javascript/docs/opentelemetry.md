# OpenTelemetry submission (experimental)

> **This feature is still under development and may change.**
> It is intended for suites that do not already configure OpenTelemetry.
> Existing OpenTelemetry setups may work, but are not yet supported.

When `otelEnabled` is true, spans are the only submission path. Every test
becomes a `test.execution` span with `buildkite.execution.via=otlp`, which makes
the span itself the submission: Buildkite synthesizes the execution from it
server-side, and nothing is sent to `/v1/uploads`. Resources identify the suite,
CI run, worker and VCS ref that produced the telemetry. Test Engine run metadata
and any `tags` you configure live on each `test.execution` span.

OpenTelemetry submission is off by default. Jest and Playwright support it.

## Playwright

Enable `otelEnabled` on the reporter; no fixture or test changes are needed:

```js
// playwright.config.js
module.exports = {
  reporter: [
    ['line'],
    ['buildkite-test-collector/playwright/reporter', { otelEnabled: true }],
  ],
}
```

Install the optional OpenTelemetry packages listed below. The reporter decides
the submission mode in `onBegin`, before tests run. It exports one root span
per reported attempt, including retries and skips, using Playwright's start
time and duration (including hooks), then flushes and shuts down in `onEnd`.
Export failures do not change Playwright's exit code.

Scope, name, file, line, column and `@key:value` tags use the same record as
the JSON upload. Scope is `titlePath()` joined with spaces: root suite,
project, file, describe titles and test title. Empty titles stay in, so the
empty root adds a leading space and an unnamed project adds another.
Per-test tags override reporter tags and the automatic `ci.worker.id` tag.
Each attempt gets a fresh UUID; retries keep the same scope and name.

`passed` maps to `pass`, `failed` and `timedOut` to `fail`, and `skipped` to
`skipped`. `interrupted` maps to `fail` in OpenTelemetry (the JSON path retains
its existing `unknown`). As in JSON, `expectedStatus` does not override the
actual attempt's result: expected failures are `fail`, unexpected passes are
`pass`. A failing `beforeAll` hook fails the entering test; subsequent tests
that Playwright reports as skipped get skipped spans. Failure messages and
stack traces use the JSON failure detail in an `exception` event.

The reporter runs in the main process, separate from the test workers. It
does not export child spans, phase spans, browser instrumentation or in-test
annotations, and does not install or change a global OpenTelemetry provider.
HTTP mocks inside workers do not affect its exporter; HTTP mocks in the main
process must allow the OTLP endpoint.

The endpoint, token precedence, run-key validation, setup fallback, batch/queue
limits and `TEST RESULTS MISSING: OpenTelemetry dropped N…` warning described
below also apply to Playwright. Jest's worker-environment and end-of-file flush
behaviour does not: Playwright exports from the reporter process and flushes
in `onEnd`. Its `buildkite.test.framework.name` is `playwright` and
`buildkite.test.framework.version` is Playwright's version.

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

`buildkite-test-collector/jest/environment` extends `jest-environment-node`.
If your suite needs another environment, build one with `extend`:

```js
// buildkite-jsdom-environment.js
const { extend } = require('buildkite-test-collector/jest/environment')
module.exports = extend(require('jest-environment-jsdom').default)
```

```js
// jest.config.js
testEnvironment: './buildkite-jsdom-environment.js',
```

The reporter options (`token`, `tags`, `otelEnabled`) are the same as before.
`otelEnabled` can also come from the environment, for example
`otelEnabled: process.env.BUILDKITE_OTEL === 'true'`.

Export needs the OpenTelemetry packages, which are optional peer dependencies
of `buildkite-test-collector` and are not installed with it:

```bash
npm install --save-dev @opentelemetry/api @opentelemetry/sdk-trace-base \
  @opentelemetry/resources @opentelemetry/exporter-trace-otlp-proto
```

The collector supports `@opentelemetry/api` 1.9+, `@opentelemetry/sdk-trace-base`
and `@opentelemetry/resources` 2.x, and `@opentelemetry/exporter-trace-otlp-proto`
0.200 or newer. Those SDK versions need Node.js 18.19 or 20.6 and newer. The
packages are only loaded once a run has decided to use OpenTelemetry; with
`otelEnabled` off nothing changes.

### What a run looks like

Each test that Jest runs gets one `test.execution` span, which is a trace of its
own: the span is never nested under anything else. A test retried with
`jest.retryTimes` gets one span per attempt. A test skipped with `test.skip`,
`xit` or `test.todo` gets a `skipped` span. When a `beforeAll` hook fails, every
test in its block gets a failed span carrying the hook's error, which is also
what the JSON upload reports.

The span's start and end are read from the real clock in the worker, outside
the test's sandbox, so `jest.useFakeTimers()` does not affect the execution's
duration. The result is Jest's own verdict for the test, read once the test has
settled, so `afterEach` failures count against the test as they do in Jest's
output.

Spans are exported with explicit settings on a provider the collector owns:
the sampler always samples, span and trace IDs come from `crypto.randomBytes`
(so a suite that stubs `Math.random` cannot produce colliding IDs), and the
attribute and event limits below apply whatever `OTEL_SPAN_*` is set to.

On Buildkite Agent v3.110 or newer with tracing enabled, the test span links to
the Agent's job span through `TRACEPARENT`, letting you navigate between them
without combining every test into one trace.

Child spans (what the test did, and where its time went) are not exported for
Jest: the test file runs in a separate module realm from the environment, so an
`@opentelemetry/api` instance inside a test cannot see the collector's span.

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

A failed test sets the span's status to error with the failure's first line as
its description (at most 1,024 characters). Each failure is an `exception`
event whose `exception.message` carries the rest of Jest's failure message,
including the stack trace, truncated to 10,243 characters.

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
`https://tests-otlp.buildkite.com/v1/traces`) and authenticates with
`BUILDKITE_TESTS_OTLP_TOKEN` or, when that is unset or blank, the reporter's
`token` option or `BUILDKITE_ANALYTICS_TOKEN`: either the suite API token or an
agent OIDC token with the `write_uploads` scope. bktec's OTLP relay sets both
`BUILDKITE_*` variables to its loopback listener and local credential, then
forwards spans to Buildkite with its own credential.

Every request is an OTLP/HTTP protobuf `POST` with gzip compression and the
headers `Authorization: Token token="…"`, `Buildkite-Tests-Run-Key` (the run
key) and `Buildkite-Tests-Span-Stream: test`. If you relay collector traffic
through your own OTLP proxy, forward both `Buildkite-Tests-*` headers.

The standard `OTEL_EXPORTER_OTLP_*` endpoint, header, compression, certificate
and protocol variables are never read, so credentials the process configures
for another OpenTelemetry destination are not sent to Buildkite, and that
destination's settings do not redirect test results. `OTEL_TRACES_SAMPLER`,
`OTEL_BSP_*`, `OTEL_SPAN_*` and `OTEL_EVENT_*` are ignored for the same reason.

Without either token, nothing is exported and the run uses the JSON path as it
always has (which also does not upload without a token), so a suite that
hard-codes `otelEnabled: true` stays quiet on a developer machine.

Each worker exports its spans in batches of up to 120, with a queue of 8,192
spans, a 1,000 ms schedule delay and a 30-second export timeout. Set
`BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` or
`BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` to override the batch or queue size.
Overrides must be positive decimal integers no greater than 2,147,483,647;
invalid values warn and use the corresponding default, and a batch size larger
than the queue size warns and resets both to their defaults. The server rejects
request bodies over 900 KiB, so lower the batch size rather than raise it if a
suite produces very large failure output.

Attribute values are truncated to 10,243 characters, each span keeps at most
128 attributes and 100 events, and each event keeps at most 128 attributes.
The attribute limit matches the server, which truncates test names to 10,240
characters before deriving the test ID, so a long test name keeps the same
identity and history as the JSON upload.

The worker flushes its queue at the end of every test file, because Jest
exits workers without any later hook. Flushes in one worker share a 30-second
budget: a healthy endpoint never consumes it, but once a failing endpoint has
used it up, later flushes stop waiting and export continues in the background
until the worker exits. The exporter retries on 429, 502, 503, 504, timeouts
and connection errors with backoff; other responses (400, 401, 403, 404, 413)
fail the batch immediately.

## When something goes wrong

The run key (`BUILDKITE_ANALYTICS_KEY`, or the key derived from your CI's
variables) must be 1–255 printable ASCII characters without spaces. An invalid
key disables OpenTelemetry export with a warning before any OpenTelemetry code
is loaded, and the run falls back to the JSON upload. That upload still needs
`BUILDKITE_ANALYTICS_TOKEN`; when the only credential was
`BUILDKITE_TESTS_OTLP_TOKEN` (as with bktec's relay), the warning says that
results will not be uploaded instead.

If the OpenTelemetry packages are missing or the exporter cannot be built, the
reporter warns `OpenTelemetry span export disabled: …` followed by
`otelEnabled is set, but OpenTelemetry could not be configured …` and the run
uploads JSON, exactly as it does with `otelEnabled` off. This decision is made
once in the main process before any worker starts, so every worker in a run
uses the same path.

Test files that run under a `testEnvironment` other than the collector's (or
one built with its `extend`) cannot export spans. The reporter warns once per
environment and uploads those files' results as JSON. A run therefore never
sends the same test over both paths, but check the warning if a project uses
per-file `@jest-environment` docblocks. The same fallback applies to the whole
run under the legacy `jest-jasmine2` test runner, which does not send test
events to the environment; export needs the default `jest-circus` runner.

OpenTelemetry errors never fail a test or change Jest's exit code. If a worker
cannot build its export session, or a span cannot be started, the collector
warns `TEST RESULTS MISSING` for the affected tests and the run continues. When
the endpoint rejects a batch, the end-of-file flush warns
`TEST RESULTS MISSING: OpenTelemetry dropped N test.execution span(s) (export-failure, last OTLP failure: …)`,
naming the HTTP status or connection error (for example `403` when OTLP ingest
is not enabled for the organization, `404` for a wrong endpoint, or
`ECONNREFUSED`). Spans still queued when the worker exits are reported the same
way. Because `test.execution` spans are the submission, a dropped span is a
test execution Test Engine never sees.

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
