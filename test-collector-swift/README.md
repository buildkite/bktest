# Buildkite Test Collector for Swift

Official [Buildkite Test Engine](https://buildkite.com/platform/test-engine/) collector for Swift test frameworks ✨

⚒ **Supported test frameworks:** XCTest, Quick, and Nimble. XCTest only:
Swift Testing (`@Test`) results are not collected.

📦 **Supported CI systems:** Buildkite, GitHub Actions, CircleCI, Xcode Cloud, and others via the `BUILDKITE_ANALYTICS_*` environment variables.

Each test execution is exported as a parentless OpenTelemetry `test.execution`
root span. Sampled OpenTelemetry spans created by the code under test can appear
as its children. Test execution data is submitted only through OTLP; the
collector does not use the legacy proprietary execution upload API.

The collector requires Swift 5.10 or newer. Its macOS deployment target is macOS
12 or newer.

## 👉 Installing

> **OTLP ingestion must be enabled for your Buildkite organization.** The
> collector sends results only over OTLP. Until ingestion is enabled, every
> upload is rejected (a `403` when OTLP ingest is not enabled for the
> organization), no results appear in Test Engine, and the collector reports
> them as missing. Your tests still run and keep XCTest's exit code. The first
> rejected test prints:
>
> ```
> [BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 403): testSubtractFails. OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.
> ```
>
> and the end of the run prints the total, for example `TEST RESULTS MISSING:
> OpenTelemetry dropped 5 test.execution span(s) so far this run`. Under
> `swift test --parallel` these lines are hidden unless a test in the same
> worker fails; see the note on parallel runs in Step 2.

### Step 1

[Create a test suite](https://buildkite.com/docs/test-analytics), and copy the API token that it gives you.


#### Swift Package Manager
 
To use the Buildkite Test Collector with a SwiftPM project, add this repository to the `Package.swift` manifest and add `BuildkiteTestCollector` to any test target requiring analytics:

```swift
let package = Package(
  name: "MyProject",
  dependencies: [
    .package(url: "https://github.com/buildkite/test-collector-swift", exact: "2.0.0-beta.3")
  ],
  targets: [
    .target(name: "MyProject"),
    .testTarget(
      name: "MyProjectTests",
      dependencies: [
        "MyProject",
        .product(name: "BuildkiteTestCollector", package: "test-collector-swift")
      ]
    )
  ]
)
```
 
### Step 2

Set the `BUILDKITE_ANALYTICS_TOKEN` secret on your CI to the API token from earlier.

By default, traces are sent using OTLP/HTTP protobuf to
`https://tests-otlp.buildkite.com/v1/traces`. The collector adds the suite token
and run key as request headers. `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` overrides
that endpoint and still receives the suite token and run key over
HTTP/protobuf. When `BUILDKITE_TESTS_OTLP_TOKEN` is set, it is sent instead of
`BUILDKITE_ANALYTICS_TOKEN`. The Buildkite Test Engine Client (bktec) relay sets
`BUILDKITE_ANALYTICS_OTLP_ENDPOINT` and `BUILDKITE_TESTS_OTLP_TOKEN` for the test
process, so tests run by bktec need no extra configuration.

To send traces to another OpenTelemetry endpoint, such as a local
OpenTelemetry Collector, leave `BUILDKITE_ANALYTICS_TOKEN`,
`BUILDKITE_TESTS_OTLP_TOKEN` and `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` unset and
use the standard exporter variables:

```bash
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="http://127.0.0.1:4318/v1/traces"
export OTEL_EXPORTER_OTLP_TRACES_HEADERS="authorization=Bearer%20local-token"
export OTEL_EXPORTER_OTLP_TRACES_PROTOCOL="http/protobuf"
```

Buildkite settings always win: while `BUILDKITE_ANALYTICS_TOKEN`,
`BUILDKITE_TESTS_OTLP_TOKEN` or `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` is set, every
`OTEL_EXPORTER_OTLP_*` variable is ignored, so an application's own
OpenTelemetry configuration cannot redirect test results away from Test Engine.
A warning names the endpoint in use unless the standard variables point at that
same endpoint, as bktec's do. Without Buildkite settings, the signal-specific
variables take precedence over `OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, and `OTEL_EXPORTER_OTLP_PROTOCOL`; a generic OTLP
endpoint has `/v1/traces` appended; and the collector logs the endpoint it
sends to. Standard endpoints receive only the explicitly configured OTLP
headers, never the suite token. Header and protocol variables alone do not
enable the collector or select an endpoint. Logged endpoints, including debug
output, show only the scheme, host, port and path; user info, query and
fragment are left out because they can carry credentials.

When sending direct, test execution spans export in the background when 120
are queued, or within 1 s of the first queued span. Export does not wait on the
test thread; only the final test-bundle flush waits for pending results.
A killed runner can lose roughly 1 s of completed direct results (up to about
120 with the defaults and a healthy endpoint); an outage can leave more queued.

When `BUILDKITE_TESTS_OTLP_TOKEN` is set (blank = unset), the collector instead
hands off each test synchronously to the bktec relay before returning to
XCTest. The relay replies 200 once the long-lived bktec process holds the
request, keeping acknowledged executions safe from a test-runner restart.
This costs a relay round trip per test. Detection uses only that token, not
the endpoint host: a relay configured with just `BUILDKITE_ANALYTICS_TOKEN`
uses the direct-mode batching fallback. Failed handoffs still obey backoff,
so unsent results queued during an outage can be lost on runner restart.

Both modes use an 8,192-span queue; when full, new spans are dropped with a
`TEST RESULTS MISSING` warning. The overrides below apply in both modes,
including the batch cap when a relay handoff drains a backlog after failure.

| Variable | Default | Meaning |
| --- | --- | --- |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE` | `120` | Max spans per request; lowering it does not send results sooner. |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` | `8192` | Max queued test execution spans, including an in-flight batch. |

Both overrides must contain only decimal digits and be integers from 1 to
2,147,483,647. Invalid values warn and use that setting's default. If batch
exceeds queue, both revert to their defaults with a warning. The direct-mode
1 s timer is fixed; there is no delay override, and `OTEL_BSP_*` is ignored.
A full direct-mode batch still triggers an early send. Requests also retain the
512 KiB encoded-size cap (a single larger span is sent alone) and the existing
retry/backoff rules. Child spans keep their separate 1 s batching settings
because XCTest can kill runners.

Each test process is one Test Engine run, identified by `BUILDKITE_ANALYTICS_KEY`
or, on a supported CI system, the build and job. Outside recognised CI,
`swift test --parallel` spreads tests across several worker processes, so set
one key for the whole invocation to keep them in one run:

```bash
BUILDKITE_ANALYTICS_KEY="$(uuidgen)" swift test --parallel
```

SwiftPM's parallel runner prints a worker's output only when one of its tests
fails, so under `swift test --parallel` the collector's warnings, including
`TEST RESULTS MISSING`, are hidden for workers whose tests all passed or were
skipped. `BUILDKITE_ANALYTICS_DEBUG_ENABLED` does not change that. To see them,
add `--verbose` or run without `--parallel`.

Each worker process also sends and flushes its results on its own. The final
flush makes one immediate pass without waiting out retry backoff. Each request
waits up to 10 s for a response, including requests for batches of up to 512
child spans. The bundle-end flush shares a 30 s budget across test executions
and child spans; an already in-flight request finishes first and consumes that
budget. Direct-mode tests do not wait for requests; relay-mode tests wait for
their handoff, except during backoff. During an outage, set
`BUILDKITE_ANALYTICS_ENABLED=false` to run the suite without the collector.

### Step 3

If you're testing an Xcode project there's an extra step, Xcode doesn't pass environment variables from the process to the test runner so we need to manually map them. Open your test scheme or test plan(whichever you are using) and under the environment variable section add the following entry:

key:
`BUILDKITE_ANALYTICS_TOKEN`

value:
`$(BUILDKITE_ANALYTICS_TOKEN)`

To reach the bktec relay from an Xcode test run, map
`BUILDKITE_TESTS_OTLP_TOKEN` and `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` the same way.
Map either `BUILDKITE_TESTS_OTEL_TEST_SPAN_*` override the same way when used.

If you configure a standard OTLP endpoint, map that endpoint and its associated
`OTEL_EXPORTER_OTLP_TRACES_*` header or protocol variables into the test runner
in the same way.

The same key value pair can be specified in your main bundle's `info.plist` file if you would rather specify it there. Note variables in the environment take precedent over those in the `info.plist` file.

### Step 3.5 (Optional)

When using the default endpoint, only the analytics token is required. Supported
CI platforms can pass extra information to enrich the reports, such as commit
messages, branch names, and build numbers. Open your test scheme or test plan
again and add the following key-value pairs for your CI platform.

**Buildkite**

```
Key: BUILDKITE_AGENT_ID, Value: $(BUILDKITE_AGENT_ID)
Key: BUILDKITE_BUILD_ID, Value: $(BUILDKITE_BUILD_ID)
Key: BUILDKITE_BUILD_URL, Value: $(BUILDKITE_BUILD_URL)
Key: BUILDKITE_BRANCH, Value: $(BUILDKITE_BRANCH)
Key: BUILDKITE_COMMIT, Value: $(BUILDKITE_COMMIT)
Key: BUILDKITE_BUILD_NUMBER, Value: $(BUILDKITE_BUILD_NUMBER)
Key: BUILDKITE_JOB_ID, Value: $(BUILDKITE_JOB_ID)
Key: BUILDKITE_STEP_ID, Value: $(BUILDKITE_STEP_ID)
Key: BUILDKITE_MESSAGE, Value: $(BUILDKITE_MESSAGE)
Key: BUILDKITE_ORGANIZATION_SLUG, Value: $(BUILDKITE_ORGANIZATION_SLUG)
Key: BUILDKITE_TEST_ENGINE_SUITE_SLUG, Value: $(BUILDKITE_TEST_ENGINE_SUITE_SLUG)
Key: TRACEPARENT, Value: $(TRACEPARENT)
Key: TRACESTATE, Value: $(TRACESTATE)
```

**Circle CI**

```
Key: CIRCLE_BUILD_NUM, Value: $(CIRCLE_BUILD_NUM)
Key: CIRCLE_WORKFLOW_ID, Value: $(CIRCLE_WORKFLOW_ID)
Key: CIRCLE_BUILD_URL, Value: $(CIRCLE_BUILD_URL)
Key: CIRCLE_BRANCH, Value: $(CIRCLE_BRANCH)
Key: CIRCLE_SHA1, Value: $(CIRCLE_SHA1)
```

**GitHub Actions**

```
Key: GITHUB_ACTION, Value: $(GITHUB_ACTION)
Key: GITHUB_REF_NAME, Value: $(GITHUB_REF_NAME)
Key: GITHUB_RUN_NUMBER, Value: $(GITHUB_RUN_NUMBER)
Key: GITHUB_RUN_ATTEMPT, Value: $(GITHUB_RUN_ATTEMPT)
Key: GITHUB_REPOSITORY, Value: $(GITHUB_REPOSITORY)
Key: GITHUB_RUN_ID, Value: $(GITHUB_RUN_ID)
Key: GITHUB_SHA, Value: $(GITHUB_SHA)
Key: GITHUB_WORKFLOW, Value: $(GITHUB_WORKFLOW)
Key: GITHUB_ACTOR, Value: $(GITHUB_ACTOR)
```

**Xcode Cloud**

```
Key: CI_COMMIT, Value: $(CI_COMMIT)
Key: CI_BUILD_NUMBER, Value: $(CI_BUILD_NUMBER)
Key: CI_BUILD_ID, Value: $(CI_BUILD_ID)
Key: CI_WORKFLOW, Value: $(CI_WORKFLOW)
Key: CI_BRANCH, Value: $(CI_BRANCH)
Key: CI_PULL_REQUEST_HTML_URL, Value: $(CI_PULL_REQUEST_HTML_URL)
```

The same key value pairs can be specified in your main bundle's `info.plist` file if you would rather specify them there. Note variables in the environment take precedent over those in the `info.plist` file.
### Step 4

Push your changes to a branch, and open a pull request. After a test run has been triggered, results will start appearing in your Test Engine dashboard.

```bash
git checkout -b add-buildkite-test-engine
git commit -am "Add Buildkite Test Engine"
git push origin add-buildkite-test-engine
```

## 🏷 Tagging

You can tag test executions with key-value pairs to filter and group results in [Test Engine](https://buildkite.com/docs/test-engine/test-suites/tags).

### Run-level tags

Run-level tags apply to all test executions in a run. Set the `BUILDKITE_ANALYTICS_TAGS` environment variable to a JSON object:

```bash
export BUILDKITE_ANALYTICS_TAGS='{"host.arch":"arm64","cloud.region":"us-east-1"}'
```

If you're using an Xcode project, add this to your test scheme or test plan environment variables like the other `BUILDKITE_ANALYTICS_*` variables. The environment variable is the only way to set run-level tags; the collector loads itself before any test code runs, so there is no programmatic hook.

Run tags are exported as `buildkite.tag.<key>` attributes on every test root.
An execution-level tag takes precedence when it uses the same key.

### Execution-level tags

Tag individual tests from within a test method using the `tagExecution` extension on `XCTestCase`:

```swift
class PaymentTests: XCTestCase {
    func testChargeCard() {
        self.tagExecution("suite", "smoke")
        self.tagExecution("feature", "payments")

        // ... test code ...
    }
}
```

The existing `tagExecution` API is unchanged. Execution tags are exported as
`buildkite.tag.<key>` attributes on that test's root span.

## OpenTelemetry behavior and delivery

The collector creates each `test.execution` span through a private AlwaysOn
provider, so it remains a trace root regardless of an application's sampling
configuration. When Buildkite Agent trace context is available, the execution
links to the job span instead of becoming its child. If the test suite has an
OpenTelemetry SDK provider, the collector adds a forwarding processor without
replacing its sampler or exporters. Otherwise, it installs a provider so spans
created under a test can be exported as execution children.

Resources identify producer-wide entities: the suite, provider-native CI run
and URL, Buildkite worker, and checked-out VCS ref. Test Engine run metadata,
collector and framework details, custom run metadata, and run tags are attached
only to each `test.execution` root. They are not duplicated onto child spans.
Collector-managed children share the producer resource; children from a tracer
provider configured by the test suite keep that provider's resource.

The collector prioritizes `buildkite.execution.via`, `buildkite.run_key`, and
`test.case.result.status` so Test Engine can synthesize an execution when the
OpenTelemetry SDK span attribute limit is constrained.

While the endpoint is healthy, finishing a test waits for its root span to be
accepted by the configured OTLP endpoint. After a transient failure (a network
error, timeout, or an HTTP 408, 429, 502, 503 or 504 response), subsequent
roots stay in the collector's in-memory queue while export attempts back off
for 10, 20, 30, and then at most 60 seconds. The bundle-end flush makes one
immediate final attempt without waiting out the backoff; each request times
out after 10 s, and the whole flush is capped at 30 s. Any other HTTP error
(for example 401 or 403 for a bad token, or 413 for an oversized request) is not
retried: the queued executions are dropped and a `TEST RESULTS MISSING` warning
names the status. When the endpoint is the Test Engine Client relay, acceptance
transfers the span to the longer-lived parent process, which protects completed
executions from an XCTest runner restart.

This is not disk-backed delivery. A hard exit before a test finishes, a process
exit while the endpoint remains unavailable, or a machine restart can still
lose spans. Drops the collector sees before the bundle-end flush are reported
there as a `TEST RESULTS MISSING` total; losses from a hard exit or restart are
not reported. An ambiguous network timeout may also result in a retry after the
server accepted the first request.

## 🔍 Debugging

To enable debugging output, set the `BUILDKITE_ANALYTICS_DEBUG_ENABLED` environment variable to `true`. This also needs
to be set in your test scheme or test plan if you're using an Xcode project.

The library uses the presence/absence of certain environment variables to determine which CI platform it's running on, if you turn on debugging you'll see the library looking and not finding some of these keys, this is intended behaviour. Failing to find the values for your CI platform would indicate an issue though.

## 🔜 Roadmap

See the [GitHub 'enhancement' issues](https://github.com/buildkite/bktest/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement) for planned features. Pull requests are always welcome, and we’ll give you feedback and guidance if you choose to contribute 💚

## 👩‍💻 Contributing

Contributions are welcome! Please read our [contributing guidelines](CONTRIBUTING.md).

## 🚀 Releasing

Releases are generated from the monorepo and mirrored to the standalone
SwiftPM repository. See the monorepo's
[collector release guide](https://github.com/buildkite/bktest/blob/main/RELEASING.md#swift-swift-package-manager).

## 📜 License

The package is available as open source under the terms of the [MIT License](https://github.com/buildkite/bktest/blob/main/test-collector-swift/LICENSE).
