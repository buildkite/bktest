# OpenTelemetry submission (experimental)

> **This feature is still under development and may change.**
> This experimental support is intended for suites that do not already configure
> OpenTelemetry. Existing OpenTelemetry setups may work, but are not yet
> supported or guaranteed to work.

When OTel mode is on, spans are the only submission path. Every test that
pytest runs gets a `test.execution` span carrying `buildkite.execution.via=otlp`,
which makes the span itself the submission: Buildkite synthesizes the execution
from it server-side, and nothing is sent to `/v1/uploads`. A `--json` file is
still written when you ask for one, because `bktec` reads it; it is not an
upload.

OTel mode is off by default. Turn it on with an environment variable and
install the optional OpenTelemetry dependencies:

```sh
pip install "buildkite-test-collector[otel]"
export BUILDKITE_ANALYTICS_OTEL_ENABLED=true
pytest
```

The `otel` extra installs `opentelemetry-sdk` and
`opentelemetry-exporter-otlp-proto-http` (1.45 or newer: older exporters
retried every 5xx, which Buildkite's receiver must never see). Without them,
or with an older exporter, OTel mode warns at the start of the session and
uploads the run's results as JSON instead, exactly as it does with the switch
off.

## What a trace looks like

Each test gets a `test.execution` span of its own. Under it are up to three
phase spans, and any spans the test records on the global tracer provider nest
under the phase they ran in:

```text
test.execution  "tests/test_pipeline.py::test_create_build"   12.4ms
├── test.setup                                                   2.0ms
│   └── INSERT  pipelines                                        1.2ms
├── test.body                                                    9.9ms
│   ├── GET     api.example.com                                  8.1ms
│   └── SELECT  builds                                           0.9ms
└── test.teardown                                                0.5ms
    └── DELETE  pipelines                                        0.4ms
```

`test.setup` covers fixture setup, `test.body` the test function, and
`test.teardown` fixture finalizers. A phase that raises gets an error status
and an `exception` event, so a trace shows which phase failed before you read
the test's own error. A fixture that raises leaves no `test.body` span, because
pytest never calls the test. A phase that skips the test (`pytest.skip`, a
`skip` marker, `pytest.xfail`) is not a failure and keeps its status unset.

A phase is exported only when it grouped at least one span or has a status; a
phase with nothing in it says nothing the test span does not. A test with no
instrumented work therefore adds no child spans at all, and the span filter
never sees phase spans.

One test is one trace. Child spans share the root's trace ID, and the root is
never nested under anything else, so a trace always belongs to exactly one
test. On Buildkite Agent v3.110 or newer, the test span links to the Agent's
propagated job trace (`TRACEPARENT`) when tracing is enabled, letting you
navigate between them without combining every test into one trace.

Results are the same verdicts the JSON upload reports: `pass`, `fail` (any
phase failed, including a strict `xfail` that passed), or `skipped` (a skip or
an `xfail` that failed as expected). A test that fails to import produces one
failed execution per collection error, timed when pytest reports it, with no
phase spans.

Under pytest-xdist each worker process exports its own spans and the
controller exports nothing, so one run still contains every test. Each worker
decides on its own: a worker that cannot configure OpenTelemetry (for example
without the SDK) warns and uploads its own tests as JSON. Each worker
flushes before it reports its session finished, so the controller waits for
it. A worker's `TEST RESULTS MISSING` warning appears in the controller's
output like any other worker stderr, and the controller repeats what each
worker could not upload after pytest's summary (`TEST RESULTS MISSING:
OpenTelemetry on xdist worker gw1 did not upload 4 test.execution span(s)
(...)`). A worker that crashes never reports; its tests are missing from the
run without a warning, as with the JSON upload. Under
pytest-forked the test span is recorded by the parent process; phase and child
spans recorded inside the forked child are lost with it.

Test and phase spans are timed with `clock_gettime(CLOCK_REALTIME)`, which
freezegun does not patch, so a test that freezes time does not move its span.
Windows has no such clock; there a frozen clock still moves the span.

OTel mode costs about 0.1 ms per test, nearly all of it the SDK creating and
batching each test's spans. On a suite of 5,000 empty tests that is roughly 45%
more wall time (1.3 s with the JSON upload, 1.8 s with OTel mode); on real
tests it is usually lost in the noise.

## OTLP execution attributes

Resources identify the producer and apply to every span the collector exports:

| Resource attribute | Value | Execution field |
| --- | --- | --- |
| `service.name` | the Test Engine suite slug, when available | — |
| `service.namespace` | the Buildkite organization slug, when available | — |
| `cicd.pipeline.run.id` | the CI pipeline run ID | build ID on Buildkite |
| `cicd.pipeline.run.url.full` | the CI pipeline run URL | URL |
| `cicd.worker.id` | the Buildkite Agent ID, when available | — |
| `process.runtime.version` | the Python version | language version |
| `vcs.ref.head.name` | the branch (or tag) name | branch |
| `vcs.ref.head.revision` | the commit SHA | commit |
| `vcs.ref.type` | `branch` or `tag` | — |

Each test span carries the execution itself and the run metadata that its
child spans do not need:

| Span attribute | Value | Execution field |
| --- | --- | --- |
| `buildkite.execution.via` | `otlp` — opts this span in to synthesis | — |
| `buildkite.run_key` | the Test Engine run key (required) | run key |
| `buildkite.run_url` | `BUILDKITE_ANALYTICS_URL` when it differs from the CI resource URL | URL |
| `buildkite.build_number` | the build number | number |
| `buildkite.job_id` | the job's UUID | job ID |
| `buildkite.step_id` | the step's UUID | step ID |
| `buildkite.message` | the commit message | message |
| `buildkite.collector.name` | this package's name | collector |
| `buildkite.collector.version` | this package's version | version |
| `buildkite.test.framework.name` | `pytest` | — |
| `buildkite.test.framework.version` | the pytest version | — |
| `buildkite.test.scope` | the test's module, class or nodeid prefix | scope |
| `buildkite.test.name` | the test function name with its parameters | name |
| `test.suite.name` | the test's scope | — |
| `test.case.name` | `scope::name` | — |
| `code.file.path` | the file the test is in, relative to the working directory | file name, location |
| `code.line.number` | the test's line number | location |
| `test.case.result.status` | `pass`, `fail`, `skipped` | result |
| `buildkite.test.execution.external_id` | the execution's collector-generated ID | external ID |
| `buildkite.tag.<key>` | each `execution_tag` marker and the `ci.worker.id` tag | execution tag `<key>` |

Scope, name and location are the same values the JSON upload sends, so a test
keeps its identity and history when you switch modes. A failed test sets the
span's status to error with the failure summary as its description, and adds
one `exception` event per failure; the server maps these back to the
execution's failure reason and expanded failure detail.

The `spans` fixture still works in OTel mode: `annotation` spans become
`test.annotation` events on the test span. The `http`, `sql` and `sleep`
sections have no OTLP equivalent and are not sent; record them as
OpenTelemetry spans instead (see below).

## Child spans and instrumentation

The collector exports child spans from the global tracer provider. If nothing
has set one when the session starts, the collector installs its own (with
CSPRNG span IDs and the producer resource above) and leaves instrumentation to
you: install the `opentelemetry-instrumentation-*` packages you want and
activate them in `conftest.py`, before the session starts:

```python
# conftest.py
from opentelemetry.instrumentation.requests import RequestsInstrumentor

RequestsInstrumentor().instrument()
```

Spans your tests create by hand with `opentelemetry.trace.get_tracer(...)`
are exported the same way.

If your suite already configures a `TracerProvider` and sets it with
`opentelemetry.trace.set_tracer_provider` before the session starts, the
collector attaches its forwarder to that provider instead and installs nothing.
Child spans then keep that provider's resource, sampler and limits, and any
exporter of your own attached to it also receives the `test.setup`,
`test.body` and `test.teardown` spans without their `test.execution` parent.
A provider that cannot accept span processors disables child export with a
warning; test spans are still exported.

"Before the session starts" means when a `conftest.py` that pytest loads at
startup is imported (the one in the rootdir, or in a test path you pass on the
command line), in a plugin, or before pytest runs. A provider set later, in a
fixture or in a `conftest.py` pytest only finds while collecting, is refused by
OpenTelemetry because the collector's provider is already installed: your
exporters then receive no spans, and the collector warns
`trace.set_tracer_provider() was called after the collector installed its own
tracer provider` when the session ends.

## Filtering child spans

Implement the `pytest_buildkite_otel_span_filter` hook in `conftest.py` to
decide which finished child spans are forwarded. It receives a
`ReadableSpan` and returns `False` to drop it; `True` or `None` keeps it. It
runs synchronously as each span finishes, so it should return quickly.

```python
# conftest.py
from opentelemetry.trace import StatusCode

MINIMUM_DATABASE_DURATION_NS = 200_000


def pytest_buildkite_otel_span_filter(span):
    if span.instrumentation_scope.name != "opentelemetry.instrumentation.psycopg":
        return True
    return (
        span.status.status_code is StatusCode.ERROR
        or span.end_time - span.start_time >= MINIMUM_DATABASE_DURATION_NS
    )
```

The filter only sees child spans that belong to a test; `test.execution` and
the phase spans are never filtered. A broken filter never costs you spans: if
it raises, the collector retains the span and warns on the first failure.
Dropping a span does not drop its children, so prefer filtering leaf spans such
as database calls.

## What gets sent

The collector exports to `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` (default
`https://tests-otlp.buildkite.com/v1/traces`) and authenticates with
`BUILDKITE_TESTS_OTLP_TOKEN` or, when that is unset or blank,
`BUILDKITE_ANALYTICS_TOKEN`: either the suite API token or an agent OIDC token
for the suite with the `write_uploads` scope. Without either token nothing is
exported and the run is left on the JSON path, which logs its usual
`No BUILDKITE_ANALYTICS_TOKEN environment variable present` warning and nothing
about OpenTelemetry, so a suite that sets `BUILDKITE_ANALYTICS_OTEL_ENABLED`
behaves on a developer machine exactly as it did before.

Every request carries `Buildkite-Tests-Run-Key` (the run key, from
`BUILDKITE_ANALYTICS_KEY` or the detected build ID) and
`Buildkite-Tests-Span-Stream`: `test` for batches of `test.execution` spans and
`child` for everything else, including the phase spans. A request never mixes
the two. If you relay collector traffic through your own OTLP proxy, forward
both headers.

The standard `OTEL_EXPORTER_OTLP_*` endpoint, header, compression, certificate,
client-certificate and protocol settings are never read, so credentials the
process configures for another OpenTelemetry destination are not sent to
Buildkite, and that destination's settings do not redirect test results.
Compression is pinned to gzip and the body is protobuf. TLS uses `requests`'
default trust store; behind a TLS-inspecting proxy, point `REQUESTS_CA_BUNDLE`
at the proxy's CA. The exporter retries a batch on 429, 502, 503 and 504
(honouring `Retry-After`) and on connection errors; any other status fails the
batch at once. Redirects are not followed and count as a failed export, so
`BUILDKITE_ANALYTICS_OTLP_ENDPOINT` must be the final `http(s)` URL.

The exporter sends with `requests`. Test suites that intercept `requests`
process-wide (`responses`, `requests-mock`, `vcrpy`) also intercept the
collector's export unless they pass through the endpoint host; the resulting
failures are reported as `TEST RESULTS MISSING` at the end of the session.
Likewise the `OTEL_EXPORTER_OTLP_*` variables are hidden from the process
environment for the instant the exporters are built at session start, so a
thread reading them at exactly that moment would miss them.

`test.execution` spans have a reserved, faster-draining queue and exporter.
Child spans use a separate queue and exporter, so a child flood or a rejected
child request cannot displace or poison test spans. When the session finishes,
both queues share one 30-second flush budget, test spans first. The process
exit handler then shuts the exporters down with a further 30-second budget, so
a receiver that accepts connections but never answers can hold the process for
up to 60 seconds in total before it exits with pytest's own status. Python
3.12.0 to 3.12.2 do not let an exit handler start a thread, so there each
exporter's shutdown runs on the exiting thread with its own 30-second limit,
and the exit can take up to 30 seconds longer.

## Test span sizes and batching

The collector exports `test.execution` spans in batches of up to 120, with a
queue of 8,192 spans, a 1,000 ms schedule delay and a 30-second export timeout.
Child spans use batches of up to 512, a queue of 2,048, and a 5,000 ms schedule
delay. Override the sizes with:

- `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` and `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE`
- `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` and `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE`

Values must be positive decimal integers no greater than 2,147,483,647; invalid
values warn and use the default. If a batch size exceeds its queue size the
collector warns and resets both to their defaults. The processors set every
option explicitly, so the SDK's `OTEL_BSP_*` variables are ignored.

The server rejects request bodies over 900 KiB (gzip). Exporting directly to
Buildkite, a suite that finishes more than roughly 1,000 tests per second for
over 10 seconds can fill the queue, after which the oldest spans are dropped
with a warning; raise the test span batch and queue sizes in that case.

Test span attribute values are truncated to 10,243 characters, event attribute
values (exception messages and stack traces) to 16,384 characters, and each
span keeps at most 128 attributes and 100 events, dropping the oldest. The
collector also caps the failure reason at 1,024 characters and each exception
message at 10,243. These limits ignore the process-wide `OTEL_SPAN_*` and
`OTEL_ATTRIBUTE_*` settings, so an application's limits cannot strip execution
identity, results or failure detail.

`OTEL_SDK_DISABLED=true` and `OTEL_TRACES_SAMPLER` are also ignored for
`test.execution` spans: with OTel mode on, test results always go out. Both
still apply to the global tracer provider, so with `OTEL_SDK_DISABLED=true`
the phase and child spans are not sent.

## When something goes wrong

OTel errors never fail the test process: pytest's exit status is always its
own. The collector warns on stderr with the `buildkite-test-collector` prefix.
Warnings raised during the session, including inside a test while pytest
captures its output, are held and printed after pytest's summary, so output
capture cannot hide them.

The run key must be 1–255 printable ASCII characters without spaces. An invalid
key disables OpenTelemetry export with a warning and the collector falls back
to the JSON upload, which still needs `BUILDKITE_ANALYTICS_TOKEN`; when the
only credential was `BUILDKITE_TESTS_OTLP_TOKEN`, the warning says that results
will not be uploaded. The same fallback applies when the OpenTelemetry packages
are missing or the SDK cannot be configured; when they are missing, the
warning names the install command (`pip install "buildkite-test-collector[otel]"`).

A test that is still running when pytest is interrupted with Ctrl-C (`SIGINT`)
is exported as skipped, since pytest never reports its result, and the session
end flush still runs. `SIGTERM` is not handled: Python exits without running
the session end or process exit flush, so queued spans are lost without a
`TEST RESULTS MISSING` warning, the same as the JSON upload is lost on
`SIGTERM` today.

Because `test.execution` spans are the submission, the collector warns
`TEST RESULTS MISSING` whenever any of them have been dropped, naming the cause:
a queue overflow (`not-exported`), an export failure with the HTTP status or
connection error (for example a `403` when OTLP ingest is not enabled for the
organization, or a `404` for a wrong endpoint), or a span that could not be
started. A span that could not be started is reported as soon as it happens;
every other drop is reported after the session-end flush and again after the
process-exit shutdown, each time with the cumulative total for the run and only
when that total has grown. Export is confirmed within the 30-second session-end
flush; spans still pending when it runs out are reported as unconfirmed, and
the process-exit shutdown gives them up to 30 seconds more before the process
ends. Nothing is reconciled after that: a span unconfirmed at exit may or may
not have arrived.

Dropped or unconfirmed child spans are reported the same way, but as
best-effort: the warning says `test.execution` results are unaffected.

One process reports one run. A refreshed token is picked up when the plugin is
configured again in the same process (for example a warm worker), but run
identity is fixed when export starts: a different run key warns and keeps
attributing results to the original run. Reporting a new run requires a new
process.
