# OTel collector spec conformance

Audit of `test-collector-swift` 2.0.0-beta against the portable collector spec
(`docs/collector-otel-mode-spec.md` on the `otel-collector-spec` branch), done October 2026.

Swift was written before the spec and has been OTLP-only since 2.0.0-beta.1: there is no
JSON upload path, every `test.execution` span is a parentless root, child spans are forwarded
from the suite's global tracer provider, and export goes through `opentelemetry-swift`'s
`OtlpHttpTraceExporter`. The audit therefore sorts every spec clause into one of:

- **conforms** — behaviour matches the spec.
- **fixed** — the deviation changed what Test Engine records or bills, so it was fixed in
  2.0.0-beta.2.
- **document** — a design difference with no effect on results or billing. Left as is; the
  table says what closing it would take.
- **N/A** — the clause describes the JSON path or a framework feature Swift does not have.

Symbols are under `Sources/Core/` unless noted.

## Clause table

| # | Clause | Swift behaviour | Verdict | Test |
|---|---|---|---|---|
| 3.1 | OTel off by default, JSON default | OTLP is the only path (`TestCollector.init` → `TelemetryClient.live`) | N/A: no JSON path in 2.x | — |
| 3.2 | SDK not set up during configure | Provider is built in `TestCollector.load` → `LiveTelemetryClient.init` at XCTest principal-class load | document: XCTest exposes no later hook before the first test; the only alternative is lazy creation on the first `testCaseWillStart` | — |
| 3.3 | Reject an uncallable span filter | No span filter feature | document (with 8.3) | — |
| 3.4 | No credential → off, silently | `CollectorOTLPConfiguration.init` returns nil; `TestCollector.init` logs `info: … requires an API token` | conforms | `CollectorTests` |
| 3.5 | Prefer `BUILDKITE_TESTS_OTLP_TOKEN`; blank = unset | Only `BUILDKITE_ANALYTICS_TOKEN` is read (`EnvironmentValues.analyticsToken`) | document: bktec's relay instead sets the `OTEL_EXPORTER_OTLP_TRACES_*` variables Swift already honours (4.2). Closing: read the new variable with precedence, ~10 lines plus a test | — |
| 3.6 | Endpoint and credential from `BUILDKITE_*`; endpoint override for development | `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` honoured with Buildkite credentials (`CollectorOTLPConfiguration.init`) | conforms | `TelemetryClientTests` |
| 3.7 | Unsupported framework → warn, JSON | XCTest only | N/A | — |
| 3.8 | Invalid run key → warn, fall back before SDK load; boundaries 1 and 255, printable ASCII | `CollectorOTLPConfiguration.isValidRunKey` (`[!-~]{1,255}`) on both the Buildkite and the relay path; invalid keys disable export with a warning before any provider exists. There is no JSON path to fall back to | **fixed** | `testDisablesExportForInvalidRunKey` |
| 3.9 | Warn when only the relay token held the credential | No relay token | document (with 3.5) | — |
| 3.10 | OTel enabled but unconfigurable → JSON | No JSON | N/A | — |
| 4.1 | Run key, stream and token headers on every request including retries | `CollectorOTLPConfiguration.headers(for:)` adds `Buildkite-Tests-Span-Stream` (replacing any configured copy, since the exporter appends header values); `TelemetryClient.live` builds one exporter per `SpanStream`, so roots and children never share a request. Headers are fixed per exporter, so retries carry them | **fixed** (previously no stream header, so child spans were billed as test spans and escaped the child cap) | `testLabelsEachSpanStreamOnTheWire` |
| 4.2 | Ignore standard OTLP env for the Buildkite endpoint; pin gzip | `OtlpHttpTraceExporter(envVarHeaders: nil)`, `compression: .gzip`. But `OTEL_EXPORTER_OTLP_{ENDPOINT,HEADERS,PROTOCOL}` and the `_TRACES_` variants select a separate, documented relay path (`EnvironmentValues.otel*`); when `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set the Buildkite credential headers are not sent | document: deliberate and README-documented (bktec's relay uses it). Risk: a vendor's `OTEL_EXPORTER_OTLP_ENDPOINT` redirects test results. Closing: delete the `otel*` reads and the relay branch of `CollectorOTLPConfiguration.init`, ~40 lines plus README | `TelemetryClientTests` |
| 4.2 | gzip | `OtlpHttpExporterBase.createRequest` upstream only compresses under `#if canImport(Compression)`, so Linux sends `identity` (the receiver accepts it; the 900 KiB request cap then applies to the raw body) | document: upstream SDK limitation; closing needs a custom `HTTPClient` or an upstream change | — |
| 4.3 | Always-on sampler, CSPRNG ids, producer resource | `makeProvider` uses `Samplers.alwaysOn`; `TracerProviderBuilder`'s default `RandomIdGenerator` uses `SystemRandomNumberGenerator` | conforms | — |
| 4.4 | Batch/queue settings independent of `OTEL_BSP_*`; overrides | Roots are exported synchronously per span (`SynchronousExecutionSpanProcessor`). Children: `BatchSpanProcessor(scheduleDelay: 1, maxQueueSize: 8192, maxExportBatchSize: 512)`, explicit, no env reads, no overrides | document: sizes differ from the spec's 2048/512/5 s and there are no `BUILDKITE_TESTS_OTEL_*_SIZE` overrides. Closing: two env reads with validation, ~30 lines | — |
| 4.5 | Span attribute limits independent of `OTEL_SPAN_*`; identity and result survive the count limit; exception attributes truncated; newest 100 events kept | `TelemetryLimit.rootSpanLimits` pins the attribute value length at 10,243 (the Swift SDK reads no env); `String.truncated(toUnicodeScalars:)` (fileprivate in `TelemetryClient+Live.swift`) caps annotation content and `exception.stacktrace` at 16,384 scalars because the SDK does not limit event attribute lengths. Synthesis attributes are written last so they survive a count limit; `buildkite.test.name` is written earlier, so at a limit below 8 it would be lost (the pinned default is 128). The SDK keeps the **oldest** 128 events, so an execution with more than 128 annotations loses its `exception` events | **fixed** (value lengths); document (event eviction order: the SDK's `ArrayWithCapacity` drops new events; keeping the newest would need the collector to buffer events until finish) | `testCapsOversizedValuesBeforeExport`, `testRetainsRequiredSynthesisAttributesAtMinimumUsefulSpanLimit` |
| 4.6 | Retry policy: never retry 400/401/403/404/413/415/500 | `SynchronousHTTPClient` classifies: transport errors and 408/429/502/503/504 are retryable; every other non-2xx is final and the processor drops the queue | **fixed** (previously every failure was retried forever, so a 400/401/403 cost a 10–60 s backoff on every later test and the results were lost without any warning) | `testDropsExecutionsRejectedByTheReceiverAndReportsTotalsAtFlush` |
| 4.6 | `Retry-After` | Not read | document: backoff is the fixed 10/20/30/60 s ladder | — |
| 4.7 | Export timeout 30 s | `OtlpConfiguration(timeout: 10)` per request; 30 s flush deadline | document: tighter per-request timeout, same flush budget | — |
| 4.8 | Name an export exception once; fail the batch | No throwing path; transport errors become `.failure` with a reason | conforms | — |
| 5 | Resource: `service.name/namespace`, `cicd.*`, `vcs.*`; run key not on the resource | `providerResource`: suite slug / org, `cicd.pipeline.run.id`, `cicd.pipeline.run.url.full`, `cicd.worker.id`, `vcs.ref.head.*`; merged with `EnvVarResource.get()` | conforms (`process.runtime.version` absent; the server ignores it) | `testExportsExecutionRoot…` |
| 6.1 | Root `test.execution`, no parent, link to the job span | `startExecution`: `setNoParent()`, `addLink` from `TRACEPARENT` | conforms | `testExecutionRemainsRoot…` |
| 6.2 | Synthesis attributes written first | Written last, because the Swift SDK keeps the newest attributes under a count limit | conforms (same outcome at the pinned default limit; see 4.5) | `testRetainsRequiredSynthesisAttributes…` |
| 6.3 | Scope/name/location, `test.case.name`, `test.suite.name` | `buildkite.test.scope` = class, `buildkite.test.name` = method, `test.case.name` = qualified name. `code.file.path`, `code.line.number` and `buildkite.test.location` come only from the first failure's location, so passing tests carry no location | document: XCTest exposes no source location for a test method. Same identity as the 0.x JSON collector (see evidence) | — |
| 6.4 | Run attributes on every span | `executionAttributes` | conforms | ✔ |
| 6.5 | Result mapping; failure → status `ERROR` + `exception` events; reason ≤ 1,024, message ≤ 10,243 | `finishExecution` / `resultStatus`; `TelemetryLimit` caps the status description at 1,024 Unicode scalars and `exception.message` at 10,240 + `...` | **fixed** (previously unbounded) | `testCapsOversizedValuesBeforeExport` |
| 6.6 | Annotations → `test.annotation` events | `annotate` | conforms | ✔ |
| 6.7 | Tests that never enter the wrapper | XCTest calls `testCaseWillStart` for every executed case; `XCTSkip` still starts and finishes | conforms | prod: skipped execution recorded |
| 6.8 | Configure-level and per-test tags; `ci.worker.id` from the agent id | Upload tags on every root; per-test via `tagExecution`. No automatic `ci.worker.id` | document: ~5 lines to add the tag from `BUILDKITE_AGENT_ID` | ✔ |
| 7 | Phase spans | None | document: XCTest gives observers no setUp/body/tearDown boundaries | — |
| 8.1–8.2 | Child forwarder | `ExecutionChildSpanProcessor` + `ExecutionTraceRegistry` | conforms | ✔ |
| 8.3 | Customer filter | None | document | — |
| 8.4 | Global provider: collector-managed vs suite-owned | `attachChildForwarder` | conforms | ✔ |
| 8.5 | Sampling | Private provider always-on; children use the suite's sampler | conforms | — |
| 9.1 | Two providers, processors, exporters | Yes; per-stream exporters since 4.1 | conforms | — |
| 9.2 | Exporter hardening | No throwing exporter path | conforms | — |
| 9.3 | VCR/WebMock exemptions | No such tooling | N/A | — |
| 9.4 | Drop accounting | `SynchronousExecutionSpanProcessor` counts dropped roots (`droppedCount`) and the last failure reason; reports once, then totals at flush; the flush starts a fresh count | **fixed** | `testDropsExecutionsRejectedByTheReceiverAndReportsTotalsAtFlush`, `testDropsExecutionsStillUnsentAtFlushAndStartsAFreshCountPerRun` |
| 10.1 | Flush at end, shutdown at exit | `testBundleDidFinish` → `forceFlush` (30 s); no shutdown | document: XCTest has no exit hook; the process ends right after the bundle | ✔ |
| 10.2 | Reconfigure rules | `TestCollector.load` is idempotent | conforms | — |
| 10.3 | Exit hook once | None | document (with 10.1) | — |
| 11.1 | Never raise into the framework | Non-throwing paths throughout | conforms | prod |
| 11.2 | Invalid run key warning | `warning: OpenTelemetry span export disabled: run key "…" is invalid…` | **fixed** | `testDisablesExportForInvalidRunKey` |
| 11.3 | `TEST RESULTS MISSING` on dropped test spans; totals at flush | `SynchronousExecutionSpanProcessor.reportDropped` | **fixed** | as 9.4 |
| 11.4 | Child drop warning | `BatchSpanProcessor` drops silently | document: upstream SDK has no drop callback | — |
| 11.5 | Warning prefix | `[BuildkiteTestCollector] <level>: …` | conforms | — |
| 12 | Server synthesis prerequisites | All present; `code.line.number` is an int | conforms | prod |
| 13 | JSON identity parity | No JSON path; production shows the same test ids as the 0.6.0 JSON collector | N/A | prod |
| 16 E2E | Loopback receiver harness | None; production runs used instead | document | — |

Counts: 19 conforms, 8 fixed rows (5 fixes), 17 document (one shared with a fixed row), 5 N/A.

## Verification on production Test Engine

Verification runs come from `buildkite/test-engine-client-examples` PR #81 (branch
`otel-swift-conformance-verify`), which adds four steps to the `swift-xctest` example: the
collector from `bktest` `main` (baseline), from this branch (conformance), and the conformance
build run without a token and against an unreachable endpoint. The example has five tests:
three pass, `FailingTests.testSubtractFails` fails, `SkippedTests.testSkipped` skips.

### Baseline — collector at `main` (2.0.0-beta.1)

Build 878: https://buildkite.com/buildkite/test-engine-client-examples/builds/878

- Test Engine run (baseline step):
  https://buildkite.com/organizations/buildkite/analytics/suites/test-engine-client-examples/runs/c43c2374-147c-86b3-9b21-117a6b7d0591
- Five executions: 3 passed, 1 failed, 1 skipped; names, locations (`FailingTests.swift:7`),
  statuses and durations match XCTest's `Executed 5 tests, with 1 test skipped and 1 failure`.
  Test ids are the same as the 0.6.0 JSON step's.
- Failed execution: `failure_reason` = `XCTAssertEqual failed: ("7") is not equal to ("999") - `,
  tags `collector.name=test-collector-swift`, `collector.version=2.0.0-beta.1`,
  `language.name=swift`, `test.framework.name=xctest`, `custom.tag.from=upload`; per-test tag
  `custom.tag.from=execution` only on `PassingTests.testAdd`.
- No token: `info: TestCollector requires an API token or OpenTelemetry exporter
  configuration. Test results will not be exported.`; five tests executed; exit 1 (XCTest's).
- Unreachable endpoint: `error: OpenTelemetry export failed for testSubtractFails; the
  execution remains queued in this process`, then at bundle end `error: OpenTelemetry export
  failed while retrying pending test executions`. No `TEST RESULTS MISSING`, no count.

### Fixed — collector at this branch (2.0.0-beta.2)

See the pull request for the build and run links at the final commit.

## Documented deviations — what closing each would take

| Clause | Cost to close |
|---|---|
| 3.5 / 3.9 relay token | Read `BUILDKITE_TESTS_OTLP_TOKEN` ahead of `BUILDKITE_ANALYTICS_TOKEN` in `EnvironmentValues`; warn at flush when it was the only credential. Small. |
| 4.2 `OTEL_EXPORTER_OTLP_*` relay path | Remove the `otel*` environment reads and the relay branch of `CollectorOTLPConfiguration.init`; README change; bktec's Swift relay would then need the `BUILDKITE_*` variables. Medium, and a product decision. |
| 4.2 gzip on Linux | Custom `HTTPClient` that gzips the body, or an upstream `opentelemetry-swift` change. Medium. |
| 4.5 event eviction order | Buffer collector-created events per execution and add the newest 100 at finish. Medium; only matters past 128 annotations on one test. |
| 4.4 batch sizes and overrides | Match 2048/512/5 s and read `BUILDKITE_TESTS_OTEL_*_SIZE` with validation. Small. |
| 4.6 `Retry-After` | Parse the header in `SynchronousHTTPClient` and feed it to the backoff. Small. |
| 4.7 export timeout | Raise `OtlpConfiguration(timeout:)` to 30 s. Trivial; left because the flush budget is already 30 s. |
| 6.3 location for passing tests | Not available from XCTest. |
| 6.8 `ci.worker.id` | Add the tag from `BUILDKITE_AGENT_ID` in `TestCollector`. Trivial. |
| 7 phase spans | XCTest offers no hook boundaries; would need `XCTestCase` subclassing by the customer. Large. |
| 8.3 span filter | New configuration surface. Medium. |
| 10.1 / 10.3 shutdown and exit hook | `atexit` shutdown after `forceFlush`. Small, but XCTest ends the process immediately after the bundle, so there is little to gain. |
| 11.4 child drop warning | Upstream `BatchSpanProcessor` has no drop callback; would need a custom processor. Medium. |
| 16 loopback harness | A test-only HTTP server decoding `ExportTraceServiceRequest`. Medium; production verification covers it for now. |
