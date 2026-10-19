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

2. Install the `buildkite-test-collector` command (Go 1.23 or newer):

   ```sh
   go install github.com/buildkite/bktest/test-collector-go/cmd/buildkite-test-collector@latest
   ```

3. Pipe `go test -json` through it in your pipeline step:

   ```sh
   set -o pipefail
   export BUILDKITE_TESTS_OTLP_TOKEN="$(buildkite-agent oidc request-token --audience https://buildkite.com/organizations/<org>/analytics/suites/<suite>)"
   go test -json ./... | buildkite-test-collector
   ```

   The command prints the tests' output exactly as `go test -v` would, so the
   job log looks the same as before, and exits 1 when a test or package
   failed and 0 otherwise. `set -o pipefail` keeps `go test`'s own exit
   status visible to the step if `go test` itself cannot run.

   Every test the stream mentions appears in Test Engine within a minute:
   one execution per test and subtest, with its package as the scope, its
   full name (`TestCheckout/empty_cart`), its file and line, its result and
   its duration. A failing test carries the output it printed (`t.Error`,
   `t.Fatal`, a panic trace) as its failure reason and detail.

   On Buildkite, GitHub Actions, CircleCI and Codeship the run key and the
   build metadata are detected from the CI environment. Elsewhere set
   `BUILDKITE_ANALYTICS_KEY` and the other `BUILDKITE_ANALYTICS_*` variables
   below. Run-level tags are a flag:
   `buildkite-test-collector -tag team=payments -tag suite=unit`.

Export is on when a token is set and the run key and endpoint are valid.
Otherwise the command prints one warning and keeps streaming the output, and
the exit code still mirrors the tests. A collector problem of any kind (bad
configuration, unreachable endpoint, export failure) is printed to stderr
prefixed with `[buildkite-test-collector]` and never changes the exit code.
If stdin holds no `go test -json` events (you forgot `-json`) the command
prints `no go test -json events on stdin` and exits 2.

A file works as well as a pipe: `buildkite-test-collector < results.json`
reads what `gotestsum --jsonfile results.json` or `go test -json > results.json`
wrote.

## Using gotestsum / keeping JUnit

The command only reads `go test -json`; it does not read JUnit XML, which
gotestsum derives from the same stream with less in it (no start times, and
a test's output and its failure merged into one message). gotestsum and a
JUnit file fit beside it either way round.

gotestsum first, when you want gotestsum's output format and JUnit for other
tools:

```sh
gotestsum --jsonfile results.json --junitfile junit.xml ./... || status=$?
buildkite-test-collector -quiet < results.json
exit "${status:-0}"
```

`-quiet` keeps the test output off stdout, since gotestsum already printed
it; warnings still go to stderr and the exit code still mirrors the tests.
The `|| status=$?` matters under `set -e`: a failing gotestsum would
otherwise end the step before anything is reported.

Collector first, when you want the `go test -v` log and still need JUnit:

```sh
set -o pipefail
go test -json ./... | tee results.json | buildkite-test-collector || status=$?
gotestsum --junitfile junit.xml --raw-command -- cat results.json
exit "${status:-0}"
```

The collector always runs here because it is the last command of the pipe,
and with `pipefail` the pipe fails whenever `go test` or the collector does,
so the status still mirrors the tests.

Either way the command reports each test under the same Test Engine test ID
as the JUnit and `go test -json` upload paths (bktec, the `test-collector`
plugin), so test history, flake detection and `bktec` splitting continue
unchanged after switching.

**Run the command or the JUnit upload, not both on the same build**: each
reports every execution, so uploading the JUnit file as well double-counts
them.

## What is reported

| Field | Value |
| --- | --- |
| scope | the package import path |
| name | the test's name as `t.Name()` reports it, including `/subtest` segments |
| location | `file:line` of the top-level test function, relative to the module root; subtests share their parent's |
| result | `pass`, `fail` or `skipped`; a test that was still running when its package died (panic in another test, `-timeout`) is `fail` |
| duration | the `Elapsed` time `go test` reports for the test; below its 10 ms resolution, the gap between the test's `run` and result events (at most 10 ms) |
| failure | the test's own output lines; the first non-empty one is the reason, the whole text is the detail, and a panic trace is attached as a stack trace |

Examples and fuzz seed corpus entries are executions like any other test.
Benchmarks and a package's own `ok`/`FAIL` lines are not executions. A
package whose test binary failed to build (or whose `TestMain` exited before
running tests) gets one warning naming it, no executions, and exit code 1.

Locations come from `go list` and the package's test files, so run the
command from inside the module (where you ran `go test`). From anywhere
else the executions have no location; set `BUILDKITE_ANALYTICS_DEBUG_ENABLED=1`
to see why. If you gave `go test` build tags, give the command the same
ones (`buildkite-test-collector -tags integration`, or `GOFLAGS=-tags=integration`
for both) so it reads the files that were compiled.

A cached package (`go test` printed `(cached)`) is replayed from the cache
and reported again, like any other result in the stream.

## Child spans, tags and annotations

The `bktest` package is optional. It reports tests from inside the test
process instead of from the `go test -json` stream, which is what lets it
attach a test's own OpenTelemetry instrumentation (database and HTTP client
spans, your own spans), per-test tags and annotations to the execution.

**Use one mode per package, not both.** A package that calls `bktest.Main`
and is also piped through `buildkite-test-collector` reports every test
twice; the command warns `package X was also exported by the bktest
library; results are duplicated` when it sees that happen, and does not try
to dedupe.

In this mode only tests that call `bktest.Start` are reported; the command
reports every test in the stream. The two modes give a test the same identity
(scope, name and location), so a suite can move between them without losing
history.

Without a token this package is silent: `Main` runs the tests and `Start`
returns a plain context, so `go test` on a laptop prints exactly what it
would without the package. With a token but an invalid run key or endpoint
`Main` prints one warning and runs the tests the same way.

```go
package checkout

import (
	"os"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m, bktest.WithTags(map[string]string{"team": "payments"})))
}

func TestCheckout(t *testing.T) {
	ctx := bktest.Start(t)                 // the test's span; child spans attach to ctx
	bktest.Tag(t, "tier", "critical")      // buildkite.tag.tier on this execution
	bktest.Annotate(t, "cart id "+cartID)  // a test.annotation event

	if err := Checkout(ctx, cartID); err != nil { // instrumented code nests under the test
		t.Fatal(err)
	}
}
```

`Main` runs the tests through `m.Run` and returns its exit code unchanged;
after export it prints `buildkite-test-collector: exported N test executions
from <package>` on stderr. `Start` ends the execution in `t.Cleanup` and
reads its result from `t.Failed()` and `t.Skipped()` once `go test` has
finished the test, so nothing else is needed in the test body and a
cleanup registered before `Start` that fails the test still counts. A
panicking test is reported as failed, and its result is delivered before
the process dies; the panic value itself is not available to a cleanup, so
the failure reads `test panicked; see go test output`. A non-panic failure
has no failure text in this mode: `go test` keeps `t.Error` output to
itself. Subtests call `Start(t)` with their own `*testing.T`. Call `Start`
after `t.Parallel()`: the span starts at `Start`, so calling it first
counts the wait for the parent as duration (the command uses `go test`'s
own elapsed time and has no such concern).

The scope is the test function's package and the location is that
function's declaration, shared by its subtests, even when `Start` is called
through a helper in another package; tests in a `package main` get the
package's import path as their scope. Subtest locations are read from the
source file the closure was compiled from, so build tags are honoured; a
subtest created by a helper in another package is looked up in the
`_test.go` files of the directory `go test` runs the binary in (under the
`-tags` the binary was built with on Go 1.24 and later, under the default
build constraints on Go 1.23), and a test binary run from another
directory reports the closure's own line instead.

`go test` caches a package's result, and a cached package runs nothing, so
it is not reported again in this mode. Changing the token, run key or
endpoint re-runs the package; pass `-count=1` to report every package on
every build (the command, which reads the replayed output, has no such
concern).

Under the test span the collector opens a `test.body` phase span on the
suite's tracer provider (the global one unless you pass `WithTracerProvider`)
and makes it the current span in `ctx`, so instrumentation that runs with
that context nests under the test and is shown against it in Test Engine.
Phase and instrumentation spans travel on a separate child stream; losing
them never loses the result. The command does not produce child spans:
nothing runs in the test process to make them.

Run-level tags, a location prefix, a child span filter and the suite's own
tracer provider are `Main` options:

```go
import sdktrace "go.opentelemetry.io/otel/sdk/trace"

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m,
		bktest.WithTags(map[string]string{"team": "payments"}),
		bktest.WithLocationPrefix("services/checkout/"),
		bktest.WithSpanFilter(func(s sdktrace.ReadOnlySpan) bool { return s.Name() != "noisy" }),
		bktest.WithTracerProvider(myProvider), // an *sdktrace.TracerProvider the suite already built
	))
}
```

Without `WithTracerProvider`, the collector attaches to the global provider
as `Main` found it when it is an SDK provider, installs its own SDK provider
(with the CI resource and secure random ids) when nobody has configured one,
and otherwise keeps exporting test spans only. It never replaces a provider
somebody chose, and leaves one installed after `Main` started (say, by a test
asserting on its own spans) entirely alone: set the suite's provider before
calling `Main` if its spans should reach Test Engine. A suite-owned provider
keeps its sampler and resource, so `OTEL_TRACES_SAMPLER` applies to child
spans; test spans always come from the collector's own provider and are
never sampled out.

## Environment variables

Both modes read the same variables.

| Variable | Purpose |
| --- | --- |
| `BUILDKITE_TESTS_OTLP_TOKEN` | Suite API token or OIDC token. Preferred over `BUILDKITE_ANALYTICS_TOKEN` when both are set. |
| `BUILDKITE_ANALYTICS_TOKEN` | Fallback token, for parity with the other collectors. |
| `BUILDKITE_ANALYTICS_KEY` | Run key; 1–255 printable ASCII characters without spaces. Defaults to the CI build id. |
| `BUILDKITE_ANALYTICS_URL`, `_BRANCH`, `_SHA`, `_NUMBER`, `_JOB_ID`, `_MESSAGE` | Override the detected CI metadata. |
| `BUILDKITE_ANALYTICS_LOCATION_PREFIX` | Prepended to every file path; `WithLocationPrefix` wins when both are set. |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | Where to send spans. Defaults to `https://tests-otlp.buildkite.com/v1/traces`. |
| `BUILDKITE_ANALYTICS_DEBUG_ENABLED` | Print why a package's test locations could not be resolved. |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE`, `_BATCH_SIZE` | Test span batch processor sizes (defaults 8192 and 120). |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE`, `_BATCH_SIZE` | Child span batch processor sizes (defaults 2048 and 512). |

The standard `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*` and
`OTEL_TRACES_SAMPLER` variables never affect `test.execution` spans or the
exporter: endpoint, headers, compression (gzip), protocol (protobuf), batch
sizes, span limits and sampling are set explicitly. Child spans follow the
sampler and limits of the provider that produced them. `OTEL_RESOURCE_ATTRIBUTES`
and `OTEL_SERVICE_NAME` are honoured through the SDK's default resource.

## Missing results

When the stream ends (or `m.Run` returns) the collector flushes within a 30
second budget. If any `test.execution` span could not be delivered it prints
`TEST RESULTS MISSING: OpenTelemetry dropped N test.execution span(s) (...)`
with the reason and the last OTLP failure, and the exit code still mirrors
the tests. Lost child spans are reported separately and never count as
missing results. A burst of results larger than the queue (thousands of
subtests finishing at once) is held back until the exporter has room,
within the same budget, rather than dropped.

Two cases lose results without that warning:

- A process killed before its first batch is exported (SIGTERM, SIGINT, the
  agent cancelling the job) loses what was queued, in both modes. The
  collector installs no signal handlers.
- The `bktest` package queues a test's result once `go test` has settled
  it, which it can only observe when the test's parent finishes (for a
  subtest whose parent called `Start`) or when `m.Run` returns (for
  top-level tests, and for subtests whose parent did not call `Start`). A
  test binary that dies before then, from `go test -timeout`, `os.Exit`, or
  a panic in a goroutine or in a test that did not call `Start`, loses the
  package's completed executions; a panic in a test that called `Start`
  is reported along with everything settled before it. The command reads
  the stream `go test` printed up to that point and reports those tests
  (the one that was running as `fail`); use it when that matters.

Export failures are reported with the token and the run key redacted, even
when the receiver echoes them back.

## What is not supported

- Child spans, tags and annotations from the command; use the `bktest`
  package for those.
- Tests that do not call `bktest.Start` when using the package; the command
  sees every test.
- `test.setup` and `test.teardown` phases. `go test` has no setup or
  teardown hooks a library can observe, so a test's trace has the
  `test.body` phase only.
- Deduplicating a package reported by both modes.
- Execution name affixes (`BUILDKITE_ANALYTICS_EXECUTION_NAME_PREFIX` /
  `_SUFFIX`) and any upload path other than OTLP.
- JUnit XML input; feed the command the `go test -json` stream the JUnit
  file was made from (see [Using gotestsum](#using-gotestsum--keeping-junit)).

## 🔍 Debugging

Collector warnings go to stderr with the `[buildkite-test-collector]` prefix.
For export problems, point `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` at a local
OTLP/HTTP receiver to see what the collector sends.

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
