# OpenTelemetry submission (experimental)

> **This feature is still under development and may change.**
> This experimental support is intended for suites that do not already configure
> OpenTelemetry. Existing OpenTelemetry setups may work, but are not yet
> supported or guaranteed to work.

When `otel_enabled` is true, spans are the only submission path. Every
`test.execution` span has `buildkite.execution.via=otlp`, which makes the span
itself the submission: Buildkite synthesizes the execution from it server-side,
with nothing sent to `/v1/uploads` and no JSON artifact written for
`artifact_path`. Resources identify the suite, CI run and worker, and VCS ref
that produced telemetry. Test Engine run metadata and any `tags:` you configure
live only on each `test.execution` span, not its children.

Every RSpec example that runs gets an OpenTelemetry `test.execution` span. The
collector can configure a provider and export instrumented child spans showing
what the test did and where its time went. The traces are sent to Buildkite and
shown against the test's execution.

OpenTelemetry submission is off by default:

```ruby
Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)
```

Export requires Ruby 3.3 or newer and the `opentelemetry-sdk` and
`opentelemetry-exporter-otlp` gems. These optional dependencies are not installed
with `buildkite-test_collector`; add them to your bundle as shown in
[Choosing instrumentation](#choosing-instrumentation).

## What a trace looks like

Each example gets a `test.execution` span of its own, with the instrumented work
it did underneath:

```text
test.execution  "Buildkite::Pipeline creates a build"   12.4ms
├── GET         api.example.com                          8.1ms
└── SELECT      pipelines                                1.2ms
```

One example is one trace. Child spans share the root's trace ID, and the root is
never nested under anything else, so a trace always belongs to exactly one test.
On Buildkite Agent v3.110 or newer, the test span links to the Agent's propagated
job trace when tracing is enabled, letting you navigate between them without
combining every test into one trace.

Three things worth knowing:

- An example skipped with `skip` produces no span at all. RSpec doesn't run its
  hooks, so there is nothing to time. `skipped` on a span means a `pending`
  example that failed as expected.
- The execution's duration is the span's duration. With export off, the legacy
  collector times the example itself as it always has.
- The result is RSpec's final verdict, read after every `around` hook has
  unwound: an `around` hook that raises after the example ran counts as a
  failure, and an example a hook marks `pending` before deliberately raising
  stays skipped.

## OTLP execution attributes

The `buildkite.execution.via=otlp` marker opts every exported test span
into execution synthesis.
Buildkite-specific execution fields are flat (`buildkite.run_key`,
`buildkite.job_id`, ...); producer identity follows OpenTelemetry resource
semantic conventions.

Resources identify entities that apply to every span emitted by their provider:

| Resource attribute | Value | Execution field |
| --- | --- | --- |
| `service.name` | the Test Engine suite slug, when available | — |
| `service.namespace` | the Buildkite organization slug, when available | — |
| `cicd.pipeline.run.id` | the CI pipeline run ID | build ID on Buildkite |
| `cicd.pipeline.run.url.full` | the CI pipeline run URL | URL |
| `cicd.worker.id` | the Buildkite Agent ID, when available | — |
| `process.runtime.version` | the Ruby version | language version |
| `vcs.ref.head.name` | the branch (or tag) name | branch |
| `vcs.ref.head.revision` | the commit SHA | commit |
| `vcs.ref.type` | `branch` or `tag` | — |

The SDK's default resource also contributes other process and `telemetry.sdk.*`
attributes. `OTEL_RESOURCE_ATTRIBUTES` remains the standard escape hatch for
additional resource identity. When a suite owns its provider, child spans keep
that provider's resource rather than the collector's.

Each test span carries the execution itself and the run metadata that its child
operations do not need:

| Span attribute | Value | Execution field |
| --- | --- | --- |
| `buildkite.execution.via` | `otlp` — opts this span in to synthesis | — |
| `buildkite.run_key` | the Test Engine run key (required) | run key |
| `buildkite.run_url` | a configured run URL when it differs from, or cannot be represented by, the CI resource URL | URL |
| `buildkite.build_number` | the build number | number |
| `buildkite.job_id` | the job's UUID | job ID |
| `buildkite.step_id` | the step's UUID | step ID |
| `buildkite.message` | the commit message | message |
| `buildkite.collector.name` | this gem's name | collector |
| `buildkite.collector.version` | this gem's version | version |
| `buildkite.location_prefix` | the raw prefix prepended to test file paths, when configured | location prefix |
| `buildkite.test.framework.name` | `rspec` | — |
| `buildkite.test.framework.version` | the RSpec version | — |
| `buildkite.test.scope` | the example group | scope |
| `buildkite.test.name` | the example's description | name |
| `test.suite.name` | the example group | — |
| `test.case.name` | the example's full description | — |
| `code.file.path` | the file the test is in | file name, location |
| `code.line.number` | the example line number, or the shared example's call-site line | location |
| `test.case.result.status` | `pass`, `fail`, `skipped` | result |
| `buildkite.test.execution.external_id` | the execution's collector-generated ID | external ID |
| `buildkite.tag.<key>` | each `configure` or `tag_execution` tag | execution tag `<key>` |

A failed test sets the span's status to error with the failure summary as its
description. Each failure is a semconv `exception` event; the server maps these
back to the execution's failure reason and expanded failure detail.

Not every JSON `run_env` field has an OTLP equivalent. The execution name
affixes (`BUILDKITE_ANALYTICS_EXECUTION_NAME_PREFIX` and
`BUILDKITE_ANALYTICS_EXECUTION_NAME_SUFFIX`) and any custom `env:` values passed
to `configure` are not sent over OTLP; `buildkite.test.name` is the example's
description as written. Use `tags:` to distinguish runs that share a suite, or
keep `otel_enabled` off if you rely on those fields.

## Choosing instrumentation

The collector configures a global SDK provider for child spans and installs the
applicable instrumentation registered when the suite starts. The private
provider still owns `test.execution`; instrumented spans use the
collector-created provider's normal sampling. The forwarding filter
excludes setup, teardown, detached traces, and other spans outside an active
execution.

You choose instrumentation by which gems you require. Add the OpenTelemetry SDK
and OTLP exporter, plus the instrumentation you want, to your bundle. Require
each instrumentation explicitly:

```ruby
# Gemfile
group :test do
  gem "opentelemetry-exporter-otlp", "~> 0.34", require: false
  gem "opentelemetry-sdk", "~> 1.13", require: false
  gem "opentelemetry-instrumentation-pg", require: false
  gem "opentelemetry-instrumentation-redis", require: false
end
```

```ruby
# spec/spec_helper.rb
require "opentelemetry-instrumentation-pg"
require "opentelemetry-instrumentation-redis"
require "buildkite/test_collector"

Buildkite::TestCollector.configure(
  hook: :rspec,
  otel_enabled: true,
)
```

A Gemfile entry makes the gem available but does not always load it. Some
applications call `Bundler.require` and auto-require their gems, but that is
host-dependent and can be disabled with `require: false`. Explicitly requiring
each instrumentation is the recommended setup.

Requiring an instrumentation gem registers its definition; it does not install
the instrumentation immediately. The collector defers OpenTelemetry setup until
RSpec's `before(:suite)` hooks and asks the SDK to install all registered
instrumentation. The SDK skips instrumentation whose target library is absent
or incompatible and reports individual installation failures without stopping
the remaining installations. To export only `test.execution` spans and any
spans your suite creates by hand, require no instrumentation gems.

If your suite already configures the OpenTelemetry SDK, the collector attaches
its forwarder to that provider and installs no instrumentation; install and
configure instrumentation there as you normally would. The collector does not
inspect instrumentation patches, so compatibility between the instrumentation
you install and other APM or test-library patches remains your responsibility.

## What gets sent

The collector merges standard `OTEL_EXPORTER_OTLP_TRACES_HEADERS` (or, when it
is absent, `OTEL_EXPORTER_OTLP_HEADERS`) over its own OTLP headers. Header names
are matched case-insensitively, so a standard `authorization` entry takes
precedence over the credential sourced from `BUILDKITE_ANALYTICS_TOKEN`. Empty
header environment variables are treated as unset.

bktec's OTLP relay uses the trace-specific header variable to provide its local
credential and forwards spans to Buildkite with its OIDC credential. Without an
OTLP Authorization header, spans go directly to Buildkite using
`BUILDKITE_ANALYTICS_TOKEN`: either the suite API token or an agent OIDC token
with the `write_uploads` scope.

With neither a token nor OTLP header variables set, nothing is exported and
`otel_enabled` is left off for the run, so a suite that hard-codes
`otel_enabled: true` stays quiet on a developer machine. This matches the JSON
path, which does not upload without a token.

OpenTelemetry's SDK owns batching, retries, and transport. `test.execution`
spans have a reserved, faster-draining queue and exporter. Forwarded children
use a separate queue and exporter, so a child flood or invalid child request
cannot displace or poison test spans. When the suite finishes, both queues
share one 30-second flush budget, test spans first. A hard exit or sustained endpoint
failure can still lose spans because the queues live in process memory.

One process reports one run. Export survives repeated suite runs in the same
process (warm workers), and a refreshed token is picked up when the collector
is reconfigured, but run identity is fixed when export starts: reconfiguring
with a different run key warns and keeps attributing results to the original
run. Reporting a new run requires a new process.

## When something goes wrong

Export never fails a test. If test span setup fails (for example on Ruby older
than 3.3, or without the OpenTelemetry gems), the collector warns and uploads
the run's results as JSON instead, exactly as it does with `otel_enabled` off.
That JSON upload needs `BUILDKITE_ANALYTICS_TOKEN`; when the credential came
only from OTLP header variables (as with bktec's relay), the warning says that
results will not be uploaded. If optional child setup or attachment fails, the collector warns, cleans up that
path, and continues exporting test spans. The suite-end flush and the
process-exit shutdown each give the OpenTelemetry SDK a 30-second budget to
export buffered spans; the SDK's own retry backoff can run past it when the
endpoint keeps failing.

Export failures are reported through OpenTelemetry's own logger. Because
`test.execution` spans are the submission, the collector also warns prominently
the first time its reserved test span queue drops any of them, usually naming
the HTTP status or connection error that caused the export to fail (for example
a `403` when OTLP ingest is not enabled for the organization, or a `404` for a
wrong endpoint). A test span that could not be started is counted and reported
the same way, since that execution also never reaches Buildkite. If more are
dropped after that, the suite-end flush and the process-exit shutdown each report the total
dropped since the last report, so a persistent failure is not mistaken for a
one-off. The suite-end flush stops at the first rejected batch and leaves the
rest queued, so when many test spans are buffered the balance drains, and is
counted, at process exit. Each suite run in a warm worker gets its own warning
and totals. Normal child-span queue overflow is not logged by the OpenTelemetry
SDK.