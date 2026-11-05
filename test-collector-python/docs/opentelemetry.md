# OpenTelemetry submission (experimental)

> **Under development and subject to change.** Intended for suites that do not
> already configure OpenTelemetry. Existing OTel setups may work, but are not
> yet supported or guaranteed to work.

## Enable OTel mode

```sh
pip install "buildkite-test-collector[otel]==1.10.0b1"
export BUILDKITE_ANALYTICS_TOKEN="your-suite-token"
export BUILDKITE_ANALYTICS_OTEL_ENABLED=true
pytest
```

Pin the beta version explicitly; normal installs and upgrades select the stable release.

The `otel` extra requires `opentelemetry-sdk` and
`opentelemetry-exporter-otlp-proto-http` 1.45 or newer. Unset
`BUILDKITE_ANALYTICS_OTEL_ENABLED` to return to JSON submission; the SDK is not
imported when the switch is off.

In OTel mode, each test is submitted as a `test.execution` span instead of a
JSON upload. Scope, name, location, results and execution tags match the JSON
path, so tests keep their identity and history. `--json` still writes a local
file for tools such as `bktec`; it does not upload it.

Missing or incompatible SDK packages, an invalid endpoint or run key, or an SDK
configuration failure cause a warning and fallback to JSON. That fallback still
requires `BUILDKITE_ANALYTICS_TOKEN`, even if an OTLP-only token was supplied.
Without either token, nothing is uploaded and the usual missing-token warning
is printed. Collector errors never change pytest's exit code.

## What gets sent

Each test starts a separate trace. Instrumented work nests beneath its
`test.setup`, `test.body` or `test.teardown` phase. Only phases that grouped a
tracked child span are sent to Buildkite, even if the filter later drops that
child. Tests without instrumented work send only the test span.

- Results follow pytest's final verdict, including teardown failures. `xfail`
  counts as skipped; strict `xpass` counts as failed. Each rerun attempt and
  reported collection error gets its own execution.
- Failures set an error status and add exception events. If teardown also
  fails, the original failure stays the reason and teardown adds another event.
- The `spans` fixture's annotations become `test.annotation` events. Its
  `http`, `sql` and `sleep` sections are not sent; use OTel instrumentation.
- On Buildkite Agent v3.110 or newer, test spans link to the propagated job
  trace (`TRACEPARENT`) when tracing is enabled.

Run metadata uses the same [overrides as JSON](../README.md#-run-metadata-overrides).
For the wire attribute mapping, see [attributes.py](../src/buildkite_test_collector/pytest_plugin/otel/attributes.py).

## Child spans and instrumentation

The collector forwards spans from the global tracer provider; it does not
automatically instrument libraries. Install the `opentelemetry-instrumentation-*`
packages you need and activate them in a startup `conftest.py`, for example:

```python
from opentelemetry.instrumentation.requests import RequestsInstrumentor

RequestsInstrumentor().instrument()
```

Spans created manually with `opentelemetry.trace.get_tracer(...)` work too.

At session start, the collector attaches to an existing suite-owned provider or
installs its own if none exists. A suite provider must be set before then: in a
plugin, before pytest runs, or when a `conftest.py` loaded at startup is imported
(in the rootdir or a test path passed on the command line). Setting it in a
fixture or a `conftest.py` discovered during collection is too late: OpenTelemetry
refuses it, its exporters receive no spans, and the collector warns at session end.

With a suite-owned provider, child spans keep its resource, sampler and limits.
Its exporters also receive all phase spans, including empty ones, but not their
`test.execution` parent. A provider that cannot accept span processors disables
child export with a warning; test spans are still exported.

### Filtering child spans

Add `pytest_buildkite_otel_span_filter` to any `conftest.py`, including one loaded
during collection. It receives a `ReadableSpan`: `False` drops it; `True` or
`None` keeps it. For example, keep database errors and slow queries:

```python
from opentelemetry.trace import StatusCode


def pytest_buildkite_otel_span_filter(span):
    if span.instrumentation_scope.name != "opentelemetry.instrumentation.psycopg":
        return True
    return (
        span.status.status_code is StatusCode.ERROR
        or span.end_time - span.start_time >= 200_000  # 0.2 ms
    )
```

The hook runs synchronously as spans finish, so keep it fast. It sees only child
spans belonging to a test, never test or phase spans. If it raises, the span is
kept and the collector warns once. Dropping a span does not drop its children.

## Configuration and transport

| Variable | Purpose |
| --- | --- |
| `BUILDKITE_ANALYTICS_OTEL_ENABLED` | `true` or `1` enables OTel mode. |
| `BUILDKITE_TESTS_OTLP_TOKEN` | Overrides `BUILDKITE_ANALYTICS_TOKEN` when non-blank. Accepts a suite API token or agent OIDC token with `write_uploads` scope. |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | Default: `https://tests-otlp.buildkite.com/v1/traces`. Must be the final HTTP(S) URL; redirects fail. |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` | Default: 120. |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` | Default: 8,192. |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE` | Default: 512. |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` | Default: 2,048. |

Sizes must be decimal integers from 1 to 2,147,483,647, with batch size no greater
than queue size. Invalid settings warn and use defaults. The run key must be
1–255 printable ASCII characters without spaces.

Requests use gzip-compressed protobuf over HTTP, retrying connection errors and
429/502/503/504 responses with backoff and `Retry-After`. Other statuses fail the
batch. A relay must forward both `Buildkite-Tests-Run-Key` and
`Buildkite-Tests-Span-Stream` (`test` or `child`); the two streams are sent separately.

Standard `OTEL_EXPORTER_OTLP_*` settings do not redirect uploads or add credentials
to them, and `OTEL_BSP_*` settings do not change batching. Test spans ignore
`OTEL_SDK_DISABLED`, `OTEL_TRACES_SAMPLER`, `OTEL_SPAN_*` and `OTEL_ATTRIBUTE_*`
settings; the global provider's settings still govern phase and child spans.

Export uses `requests`: HTTP-mocking libraries such as `responses`, `requests-mock`
and `vcrpy` must allow traffic to the endpoint. Behind a TLS-inspecting proxy, set
`REQUESTS_CA_BUNDLE` to its CA certificate.

## Missing results and shutdown

Test and child spans have separate queues, so a flood or failed batch of child
spans cannot displace test results. Queue overflow drops the oldest spans; raise
the batch and queue sizes if this happens. The receiver rejects compressed
request bodies over 900 KiB.

`TEST RESULTS MISSING` means test spans were dropped or export could not be
confirmed, with the count and cause. Child-span warnings state that test results
are unaffected. Warnings captured during tests are held until after pytest's
summary. An unconfirmed span may or may not have arrived; no later reconciliation
is performed.

At session end, both streams share a 30-second flush budget, test spans first.
Process exit allows another 30 seconds for shutdown: an unresponsive receiver
can delay exit by up to 60 seconds total. Python 3.12.0–3.12.2 can take up to
30 seconds longer because shutdown must run on the exiting thread.

- **pytest-xdist:** each worker exports and flushes its own tests; the controller
  repeats workers' missing-result warnings. A crashed worker cannot report its loss.
- **pytest-forked:** the parent exports test results, but phase and child spans
  recorded in the forked child are lost.
- **Ctrl-C:** an unfinished test is exported as skipped and the session flush runs.
- **SIGTERM:** neither flush runs; queued spans are lost without a warning.

## Other limits

- Test spans keep at most 128 attributes and 100 events, dropping the oldest.
  Span attribute values and exception messages are capped at 10,243 characters,
  event values at 16,384, and failure reasons at 1,024.
- Test and phase spans use `clock_gettime(CLOCK_REALTIME)` to avoid freezegun's
  patched clock. On Windows, freezegun still moves span times. time-machine also
  moves this clock; negative durations are clamped to zero.
- One process reports one run. Reconfiguration picks up refreshed credentials,
  but changing the run key warns and keeps the original key. Start a new process
  to report a different run.
- OTel adds roughly 0.1 ms per test in the measured 5,000-empty-test benchmark
  (about 45% wall-time overhead); real suites will vary.
