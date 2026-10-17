# Buildkite Test Collector for Go (Beta)

The official Go collector for [Buildkite Test Engine](https://buildkite.com/platform/test-engine).
It reports `go test` results as OpenTelemetry spans over OTLP, which is the
only way it sends anything: there is no JSON upload path.

📦 **Supported CI systems:** Buildkite, GitHub Actions, CircleCI, and others
via the `BUILDKITE_ANALYTICS_*` environment variables.

## 👉 Installing

1. [Create a test suite](https://buildkite.com/docs/test-engine) and copy its
   API token, or set up [OIDC](https://buildkite.com/docs/pipelines/configure/tests/test-collection/oidc)
   so each job mints a short-lived one.

2. Add the module to your project:

   ```sh
   go get github.com/buildkite/bktest/test-collector-go
   ```

   The module requires Go 1.23 or newer.

3. Run the package's tests through the collector from `TestMain`, and start an
   execution at the top of each test:

   ```go
   package calculator

   import (
       "os"
       "testing"

       bktest "github.com/buildkite/bktest/test-collector-go"
   )

   func TestMain(m *testing.M) {
       os.Exit(bktest.Main(m))
   }

   func TestAdd(t *testing.T) {
       bktest.Start(t)
       if Add(1, 2) != 3 {
           t.Error("wrong")
       }
   }
   ```

   `Main` returns `m.Run`'s exit code unchanged. `Start` returns a context
   that carries the test's span, for tests that instrument their own work
   (see [Child spans](#child-spans)); the result is recorded in `t.Cleanup` from
   `t.Failed()` and `t.Skipped()`, so nothing else is needed in the test body.
   Subtests call `Start(t)` with their own `*testing.T`, and `t.Parallel()`
   works as usual.

4. Set the environment in CI:

   ```sh
   export BUILDKITE_TESTS_OTLP_TOKEN="$(buildkite-agent oidc request-token --audience https://buildkite.com/organizations/<org>/analytics/suites/<suite>)"
   go test ./...
   ```

   On Buildkite, GitHub Actions, CircleCI and Codeship the run key and the
   build metadata are detected from the CI environment. Elsewhere set
   `BUILDKITE_ANALYTICS_KEY` and the other `BUILDKITE_ANALYTICS_*` variables
   below.

Export is on when a token is set and the run key and endpoint are valid.
Otherwise the package prints one warning and `go test` runs exactly as it would without the
collector. A collector problem of any kind (bad configuration, unreachable
endpoint, export failure) is printed to stderr prefixed with
`[buildkite-test-collector]` and never changes the exit code.

## What is reported

Every test that calls `Start` becomes one `test.execution` span:

| Field | Value |
| --- | --- |
| scope | the package import path |
| name | `t.Name()`, including `/subtest` segments |
| location | `file:line` of the function that called `Start`, relative to the module root |
| result | `pass`, `fail` (`t.Failed()`, or a panic), or `skipped` (`t.Skipped()`) |
| duration | from `Start` to the end of the test |

A test that panics is reported as failed with an `exception` event carrying
the stack trace, and its result is delivered before the process dies. A
non-panic failure has no failure text: `go test` keeps `t.Error` output to
itself, so the execution's failure reason is `test failed`; the job log has the
details.

Under the test span the collector opens a `test.body` phase span on the
suite's tracer provider (the global one unless you pass `WithTracerProvider`)
and makes it the current span in `ctx`, so any OpenTelemetry
instrumentation your test runs with that context (database clients, HTTP
clients, your own spans) nests under the test and is shown against it in Test
Engine. Phase and instrumentation spans travel on a separate child stream;
losing them never loses the result.

Per-test tags and annotations:

```go
func TestCheckout(t *testing.T) {
    ctx := bktest.Start(t)
    bktest.Tag(ctx, "team", "payments")      // buildkite.tag.team on this execution
    bktest.Annotate(ctx, "cart id "+cart.ID) // a test.annotation event
}
```

Run-level tags, a location prefix, a child span filter and the suite's own
tracer provider are `Main` options:

```go
func TestMain(m *testing.M) {
    os.Exit(bktest.Main(m,
        bktest.WithTags(map[string]string{"team": "payments"}),
        bktest.WithLocationPrefix("services/checkout/"),
        bktest.WithSpanFilter(func(s sdktrace.ReadOnlySpan) bool { return s.Name() != "noisy" }),
        bktest.WithTracerProvider(myProvider),
    ))
}
```

Without `WithTracerProvider`, the collector attaches to the global provider
when it is an SDK provider, installs its own SDK provider (with the CI resource
and secure random ids) when nobody has configured one, and otherwise keeps
exporting test spans only. It never replaces a provider somebody chose. A
suite-owned provider keeps its sampler and resource, so `OTEL_TRACES_SAMPLER`
applies to child spans; test spans always come from the collector's own
provider and are never sampled out.

## Environment variables

| Variable | Purpose |
| --- | --- |
| `BUILDKITE_TESTS_OTLP_TOKEN` | Suite API token or OIDC token. Preferred over `BUILDKITE_ANALYTICS_TOKEN` when both are set. |
| `BUILDKITE_ANALYTICS_TOKEN` | Fallback token, for parity with the other collectors. |
| `BUILDKITE_ANALYTICS_KEY` | Run key; 1–255 printable ASCII characters without spaces. Defaults to the CI build id. |
| `BUILDKITE_ANALYTICS_URL`, `_BRANCH`, `_SHA`, `_NUMBER`, `_JOB_ID`, `_MESSAGE` | Override the detected CI metadata. |
| `BUILDKITE_ANALYTICS_LOCATION_PREFIX` | Prepended to every file path; `WithLocationPrefix` wins when both are set. |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | Where to send spans. Defaults to `https://tests-otlp.buildkite.com/v1/traces`. |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE`, `_BATCH_SIZE` | Test span batch processor sizes (defaults 8192 and 120). |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE`, `_BATCH_SIZE` | Child span batch processor sizes (defaults 2048 and 512). |

The standard `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*` and
`OTEL_TRACES_SAMPLER` variables never affect `test.execution` spans or the
exporter: endpoint, headers, compression (gzip), protocol (protobuf), batch
sizes, span limits and sampling are set explicitly. Child spans follow the
sampler and limits of the provider that produced them. `OTEL_RESOURCE_ATTRIBUTES`
and `OTEL_SERVICE_NAME` are honoured through the SDK's default resource.

## Missing results

At the end of `m.Run` the collector flushes both streams within a 30 second
budget. If any `test.execution` span could not be delivered it prints
`TEST RESULTS MISSING: OpenTelemetry dropped N test.execution span(s) (...)`
with the reason and the last OTLP failure, and still returns `go test`'s own
exit code. Lost child spans are reported separately and never count as missing
results.

## What is not supported

- Tests that do not call `bktest.Start` produce nothing; the collector cannot
  see them.
- There are no `test.setup` or `test.teardown` phases. `go test` has no
  setup or teardown hooks a library can observe, so a test's trace has the
  `test.body` phase only.
- Failure text for non-panic failures (see above).
- `go test -json` post-processing. Results come from inside the test process.
- Execution name affixes (`BUILDKITE_ANALYTICS_EXECUTION_NAME_PREFIX` /
  `_SUFFIX`) and any upload path other than OTLP.

## 🔍 Debugging

Collector warnings go to stderr with the `[buildkite-test-collector]` prefix.
`go test -v` shows them next to the tests they relate to. For export problems,
point `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` at a local OTLP/HTTP receiver to see
what the collector sends.

## 🔜 Roadmap

See the [GitHub 'enhancement' issues](https://github.com/buildkite/bktest/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement+label%3Ago)
for planned features.

## ⚒ Developing

```sh
go test -race ./...
.buildkite/steps/lint   # gofmt, go vet, go mod tidy -diff
```

## 👩‍💻 Contributing

Bug reports and pull requests are welcome on GitHub at
https://github.com/buildkite/bktest.

## 📜 License

The package is available as open source under the terms of the
[MIT License](https://opensource.org/licenses/MIT).
