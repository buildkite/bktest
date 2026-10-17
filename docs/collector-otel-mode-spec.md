# Collector OTel-only mode — portable specification

**Status:** [V] Derived from `test-collector-ruby` 2.17.0 (`~/code/bktest/test-collector-ruby`, tag `test-collector-ruby/v2.17.0`) and the server at bk/bk `Analytics::API::TracesController` + ta-ingestion `OtlpExecutionSynthesizer` (local checkout Oct 1 2026). Everything tagged [V] was read in source. [P] items are proposals for the non-Ruby targets and need Matt's sign-off. ⚠️ marks gaps nobody has fixed yet.

**Source of truth:** this file is a copy of `local/otel/design/collector-otel-mode-spec.md` in the `buildkite/buildkite` repo (branch `otel-planning`). Edit there and re-copy; do not let the two diverge.

**Audience:** an AI implementer adding OTel-only mode to another Buildkite Test Engine collector (Go, JS Jest, JS Playwright, Python pytest). The doc is self-contained: it does not assume access to the Ruby repo, but names every Ruby symbol so you can read the reference if you have it.

**One-sentence summary:** in OTel-only mode the collector stops uploading JSON and instead emits one OTLP `test.execution` span per test to Buildkite; the span *is* the result submission, and optional child spans (instrumentation, phases) ride alongside on a second stream.

---

## 0. How to use this document

1. Read §1 (invariants) and §12 (server contract) first. If your implementation violates an invariant, the server silently drops the test result.
2. Implement in the order of §10 (lifecycle). Get one `test.execution` span accepted end-to-end before phases, child spans, filters or warnings.
3. Treat §16 as the acceptance test list. Port each row; do not write tests that pass against a wrong implementation.
4. §6.8 (tags), §6.9 (worked request), §10.3 (component checklist) and §14.0 (which SDK knob implements each requirement in Go/JS/Python, verified against upstream source) are the fastest route from "read" to "building". §14.1–§14.5 hold the per-target notes. They are [P] — proposals — not verified designs. Verify them against the target framework before relying on them. Items there need a *decision*, not just verification, before coding: worker-model fallback ownership (§14.1) and, for Go, test identity, the meaning of "disabled" when there is no JSON path, and global-provider ownership (§14.5).
5. Keep attribute names, header names, env var names and warning prefixes byte-identical to this doc. The server, bktec and the UI match on them.
6. Everything tagged [V] is verified against Ruby 2.17.0 or ta-ingestion source; [P] is a proposal; ⚠️ is unknown or undecided. Implement [V] as written; treat [P]/⚠️ as questions to answer and record.

---

## 1. Scope and invariants

### 1.1 What OTel-only mode is

- An opt-in configuration flag (`otel_enabled`) on the collector. **Off by default, and the default stays JSON.** With the flag off the collector must behave byte-for-byte as before: same JSON upload, same artifact file, no OTel SDK import, no new env vars read, no new warnings. [V] Ruby `otel_enabled?` gates every OTel entry point; the SDK is only `require`d inside `configure!`.
- When on and configured successfully: **no JSON upload happens** (no `/v1/uploads` POST, no artifact file). Test results reach Test Engine only as OTLP spans. [V]
- When on but the **mandatory test-span path** fails to configure (invalid run key, SDK missing, private provider setup raises): warn and **fall back to the JSON path for the whole run in that process**, decided at suite start before any test runs, so one process never has partial results in both channels. [V] The decision is **per process**: with parallel runners (`parallel_tests`, Jest workers, xdist) each process decides independently, so one server run *can* contain OTLP results from one process and JSON results from another. Ruby accepts this; worker-based targets must decide who owns the JSON fallback when workers, not the main process, do the OTel setup (§14.1).
- When only the **optional child-span path** fails to configure: warn, clean up the child path, and keep OTel mode with test-span export only. No JSON fallback. [V] `configure_child_export`
- Failures after tests have started (export errors, dropped spans) never switch to JSON; they are warned (§11). [V]
- The legacy in-process tracer (SQL/HTTP timing for the JSON `history`) is not started in OTel mode; instrumentation is expected to come from the OpenTelemetry SDK's own instrumentation libraries instead. [V]

### 1.2 Dependency model [V]

- The OTel SDK and the OTLP/HTTP protobuf exporter are **optional dependencies the customer installs**; the collector package does not depend on them (Ruby gemspec has no `opentelemetry-*` runtime dependency; the docs tell the customer to add `opentelemetry-sdk` and `opentelemetry-exporter-otlp >= 0.29` to their bundle). Importing them is **lazy**: inside the suite-start setup, never at package load. Missing or too-old SDK → the generic "disabled: <Class>: <msg>" warning + JSON fallback (§11).
- The exporter minimum version exists because the collector passes TLS/certificate options explicitly (I6) and older exporters reject the keywords. Each target must pin the minimum exporter version that accepts every explicit option it passes, and test that an older version produces the fallback warning rather than a crash.
- Runtime floor: Ruby documents 3.3 as the OTel-mode support floor (the exporter's requirement); the collector itself keeps its existing floor. The runtime fallback only covers import/setup failures *after* the package is installed; if dependency resolution rejects the runtime at install time nothing can fall back — document the supported SDK/exporter/runtime combination per target. Go's compiled dependencies raise the module's Go toolchain floor even when OTel is off; that is a §14.5 prerequisite decision.
- Per-target shape [P]: JS → `peerDependencies` marked optional in `peerDependenciesMeta`, `require()`d inside setup; Python → an extra (`buildkite-test-collector[otel]`), imported inside setup; Go → ordinary `go.mod` dependencies (compiled in; "missing SDK" cannot happen, so that fallback branch collapses to "setup raised").

### 1.3 Invariants (violate any of these and results are lost or misattributed)

| # | Invariant | Why |
|---|---|---|
| I1 | Every test span is a **trace root** (no parent) and carries `buildkite.execution.via = "otlp"`. | The synthesizer only treats parentless spans with that marker as results. [V] `OtlpExecutionSynthesizer.EXECUTION_VIA_ATTRIBUTE` |
| I2 | Every test span carries non-empty `buildkite.test.name`, a `test.case.result.status` in {`pass`,`fail`,`skipped`} (server also accepts `skip`,`pending`), `buildkite.run_key`, and `end ≥ start > 0`. | Otherwise the span is counted as "skipped" by the server and the execution is lost. [V] |
| I3 | `buildkite.test.scope` and `buildkite.test.name` are computed from the **same source values by the same code** as the `scope` and `name` the collector sends in JSON mode, so the server derives the same test ID. (The SDK truncates exported values at 10,243 chars and the server truncates back to 10,240 before deriving; that is by design, not a difference.) | Test ID = `deriveTestId(suiteId, scope, name)`. Any difference creates a new test in Test Engine. [V] |
| I4 | Each OTLP request carries exactly one span stream: header `Buildkite-Tests-Span-Stream: test` or `child`, never mixed. Two exporters, one per stream. | [V] Ruby sends stream-labelled requests. [M, D95/D112] The intended consumer is the receiver cap, which will refuse `child` requests for over-limit orgs without decoding the body; the receiver on main does not act on the header yet (§15). Test results must never share a request with child spans. |
| I5 | Test spans are exported from a **private** tracer provider the collector owns: always-on sampler, CSPRNG IDs, pinned span/event limits, its own batch processor and exporter with explicit settings. The customer's `OTEL_TRACES_SAMPLER`, `OTEL_SPAN_*`/`OTEL_EVENT_*` limits and `OTEL_BSP_*` cannot drop or strip results. | A customer's `OTEL_TRACES_SAMPLER=traceidratio` or `OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT=2` must not drop results. [V] (Ruby leaves link limits and `Resource.default` to SDK env behaviour — see §5, §9.1.) |
| I6 | Only `BUILDKITE_*` env vars configure endpoint, credential and compression. The standard `OTEL_EXPORTER_OTLP_{ENDPOINT,TRACES_ENDPOINT,HEADERS,COMPRESSION,CERTIFICATE,CLIENT_*}` vars are **never consulted** for the Buildkite exporters because every option is passed explicitly. | bktec's relay and customers' vendor exporters set `OTEL_EXPORTER_OTLP_*` to other places. [V] (Ruby does not pass `timeout:` to the exporter constructor, so `OTEL_EXPORTER_OTLP_TIMEOUT` is read as the exporter's own default; the batch processor supplies the effective 30 s export timeout per call, so it has no practical effect.) **Hazard for ports [V upstream source]:** the JS and Python OTLP/HTTP exporters *merge* `OTEL_EXPORTER_OTLP_HEADERS` / `_TRACES_HEADERS` into explicit `headers` (explicit wins per key, env-only keys are still sent). bktec sets `OTEL_EXPORTER_OTLP_TRACES_HEADERS=authorization=Bearer <relay token>` and vendors set their own auth keys. The port must guarantee the Buildkite request carries exactly its own headers — see §14.0 for the per-SDK mechanism — and test it with both env vars set to garbage. |
| I7 | The collector never raises into the test framework from its **own** code paths. Every OTel operation the collector performs (start/finish/annotate/attach/detach/flush/shutdown/forward/filter/export) rescues non-fatal exceptions, warns with the `[buildkite-test_collector]` prefix (adapt per collector), and continues. Fatal process signals (`SystemExit`, `SignalException`, `NoMemoryError` in Ruby) are always re-raised. **Exceptions from the test itself propagate unchanged** — `with_test_span` has no rescue. Public-API argument validation is the only deliberate exception and is unchanged from JSON mode: `configure` raises `ArgumentError` for an uncallable filter even when OTel is off; `tag_execution` raises for non-string arguments or when called outside a test (§3.1). | Telemetry must not fail a customer's build; the collector must also never hide a real test failure. [V] `ExceptionHandling.reraise_fatal`, `with_test_span` |
| I8 | Identity of a run is fixed from successful configure until shutdown. Reconfiguring **while configured** with a different run key warns and keeps the original; only the credential may be refreshed (best-effort). After shutdown (process exit in practice) the identity is cleared, so configure → shutdown → configure with a new key starts a new run cleanly (§10.2). | A configured exporter serves one run; the Ruby warning text says "requires a new process" because shutdown is normally only at exit. [V] `warn_run_mismatch`, `refresh_authorization`, `shutdown` |
| I9 | Dropped **test** spans are reported as "TEST RESULTS MISSING"; dropped **child** spans are reported as best-effort ("test.execution results are unaffected"). Never conflate the two. | Operators must know when results are gone. [V] `TestSpanMetricsReporter`, `ChildSpanMetricsReporter` |

---

## 2. Reference implementation map (Ruby 2.17.0)

| Concern | Ruby file / symbol |
|---|---|
| Public config, enable decision, deferred start | `lib/buildkite/test_collector.rb` — `configure`, `start_otel`, `json_fallback_outcome`, `otel_enabled?`, `annotate` |
| Core OTel module (constants, providers, lifecycle, span helpers) | `lib/buildkite/test_collector/otel.rb` — `Buildkite::TestCollector::OTel` |
| Test span attribute/result mapping | `lib/buildkite/test_collector/rspec_plugin/trace.rb` — `Trace#otel_attributes`, `#otel_result`, `#otel_failure_reason`, `#otel_exception_events` |
| Framework hooks (around each test, suite start/end) | `lib/buildkite/test_collector/library_hooks/rspec.rb` |
| Finishing the span after the framework settles the result | `lib/buildkite/test_collector/rspec_plugin/reporter.rb` — `Reporter#handle_example`, `#finish_otel_span` |
| Phase spans | `lib/buildkite/test_collector/rspec_plugin/phase_spans.rb` — `PhaseSpans` (prepended onto `RSpec::Core::Example`) |
| Child-span routing | `lib/buildkite/test_collector/otel/child_span_forwarder.rb` — `ChildSpanForwarder` |
| Customer filter wrapper | `lib/buildkite/test_collector/otel/span_filter.rb` — `SpanFilter` |
| Drop accounting / warnings | `lib/buildkite/test_collector/otel/{span,test_span,child_span}_metrics_reporter.rb` |
| CI env detection (run key, URL, branch, commit) | `lib/buildkite/test_collector/ci.rb` — `CI#env` |
| Customer docs (accurate, 466 lines) | `docs/opentelemetry.md` |
| CHANGELOG entries | 2.16.0 (first OTel-only release), 2.17.0 (span stream header, phase spans, limits) |

A second, independent OTLP-only implementation exists in Swift (`test-collector-swift` 2.0.0-beta, `TelemetryClient+Live.swift`). It uses the same attribute names and is a useful second reference for a non-Ruby shape, but Ruby 2.17.0 is the canonical behaviour.

---

## 3. Configuration interface

### 3.1 Collector options (what the customer writes in code)

Ruby signature [V]:

```ruby
Buildkite::TestCollector.configure(
  hook:,                    # framework; OTel only supported for :rspec in Ruby
  token: nil,               # JSON credential; falls back to BUILDKITE_ANALYTICS_TOKEN
  url: nil,                 # JSON endpoint; unused in OTel mode
  tracing_enabled: true,
  artifact_path: nil,       # JSON artifact; not written in OTel mode
  location_prefix: nil,     # prepended to file paths in BOTH modes
  env: {},                  # overrides for CI env (run key etc.)
  tags: {},                 # run-level tags, become buildkite.tag.* on every test span
  otel_enabled: false,      # the switch
  otel_span_filter: nil,    # callable(span) -> bool; must respond to call, else ArgumentError at configure time
)
```

Portable rule: expose `otel_enabled` (bool, default false) and `otel_span_filter` (optional predicate over a finished child span) with the same names, in the target language's idiom (`otelEnabled` / `otel_enabled` / `OTelEnabled`). Keep the existing JSON options untouched.

Per-test tagging API [V] `TestCollector.tag_execution(key, value)`: both must be strings (`ArgumentError "tag key and value expected string"` otherwise); raises `_buildkite_tags not available` outside a running test. Writes into a per-test tag map that the around-test wrapper creates before the test and hands to the trace object. The wrapper's `ensure` only drops the thread-local *binding* (so a later test cannot see it and calls outside a test raise); the map itself stays attached to the trace and is read at reporter time, becoming `buildkite.tag.<key>` on the test span. This is distinct from `annotate` (§6.6), which creates an event.

Value normalisation [V]:
- `BUILDKITE_TESTS_OTLP_TOKEN`: stripped; empty → unset.
- JSON token: explicit `token:` wins over `BUILDKITE_ANALYTICS_TOKEN`; stripped, but an empty string is **not** converted to nil (the enable check tests nil, not blankness — an empty env var therefore enables OTel with an empty credential, which the receiver rejects with 401/403).
- `BUILDKITE_ANALYTICS_OTLP_ENDPOINT`: unset → default; an explicitly empty value is used as-is (and fails at setup → JSON fallback).
- Queue/batch sizes: validly encoded, matching `\A[0-9]+\z`, numeric value in `1..2147483647`. Leading zeros accepted; signs, whitespace and decimals rejected.

### 3.2 Environment variables (what the customer or bktec sets)

| Variable | Read by | Meaning | Default |
|---|---|---|---|
| `BUILDKITE_TESTS_OTLP_TOKEN` | `OTel.api_token` | OTel credential. Preferred. Blank/whitespace = unset. bktec's relay sets this. | — |
| `BUILDKITE_ANALYTICS_TOKEN` | `configure` | Suite API token; used for OTel **only if** `BUILDKITE_TESTS_OTLP_TOKEN` is unset. Also the JSON credential. | — |
| `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` | `OTel.endpoint` | Full URL of the traces endpoint. bktec's relay points this at its loopback listener. | `https://tests-otlp.buildkite.com/v1/traces` |
| `BUILDKITE_ANALYTICS_KEY`, `_URL`, `_BRANCH`, `_SHA`, `_NUMBER`, `_JOB_ID`, `_MESSAGE` | `CI#env` | Overrides for run key, run URL, branch, commit, build number, job id, message. Non-empty values override detected CI values. | per-CI |
| `BUILDKITE_ANALYTICS_LOCATION_PREFIX` | `configure` | Same as `location_prefix` option. | — |
| `BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE` / `_BATCH_SIZE` | `span_processor_sizes` | Test-stream batch processor overrides. Invalid → warn `<NAME> must be a positive integer up to 2147483647, using default <DEFAULT>`, use default. Resolved batch > queue → warn `<BATCH_NAME> must be <= <QUEUE_NAME>; using defaults (batch <B>, queue <Q>)`, reset both. Warnings fire each time setup evaluates them (no process-wide dedupe; a reconfigure after shutdown warns again). | 8192 / 120 |
| `BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE` / `_BATCH_SIZE` | `span_processor_sizes` | Child-stream overrides, same rules. | 2048 / 512 |
| `BUILDKITE_TEST_ENGINE_SUITE_SLUG`, `BUILDKITE_ORGANIZATION_SLUG` | `producer_resource` | → `service.name`, `service.namespace` on the resource. | — |
| `BUILDKITE_AGENT_ID` | `producer_resource`, `worker_id_tag` | → `cicd.worker.id` resource attr and automatic tag `ci.worker.id`. | — |
| `BUILDKITE_STEP_ID`, `BUILDKITE_TAG`, `BUILDKITE_BUILD_ID`, `BUILDKITE_BUILD_URL`, `GITHUB_RUN_ID`, `GITHUB_REPOSITORY`, `CIRCLE_WORKFLOW_ID`, `CI_BUILD_ID` | resource/run attrs | See §5, §6.4. | — |
| `TRACEPARENT`, `TRACESTATE` | `job_span_links` | W3C context of the Agent's job span → span **link** (not parent). | — |

Run environment precedence [V] `CI#env`: detected CI metadata ← overridden by non-empty `BUILDKITE_ANALYTICS_*` values ← overridden by explicit `env:` entries (including empty/nil ones). CI detection order: Buildkite (`BUILDKITE_BUILD_ID`), GitHub Actions (`GITHUB_RUN_NUMBER`; key = `GITHUB_ACTION-GITHUB_RUN_NUMBER-GITHUB_RUN_ATTEMPT`), CircleCI (`CIRCLE_BUILD_NUM`; key = `CIRCLE_WORKFLOW_ID-CIRCLE_BUILD_NUM`), Codeship (`CI_NAME=codeship`; key = `CI_BUILD_ID`), generic (`CI` set; key = random UUID), local (key = random UUID).

Never consulted for Buildkite export [V]: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_*_CLIENT_*`, `OTEL_BSP_*`, `OTEL_SPAN_*_LIMIT`, `OTEL_EVENT_*_LIMIT`, `OTEL_TRACES_SAMPLER` (for test spans). Pass every setting explicitly to the Buildkite exporters/processors/providers so the SDK does not fall back to those env vars. (Ruby spec: "ignores standard OTLP header variables even for its own endpoint", "isolates both exporters from vendor endpoints, compression, certificates, and TLS verification switches".) Still consulted by the SDK: `OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` (via `Resource.default`, §5), link limits, and `OTEL_EXPORTER_OTLP_TIMEOUT` (inert, see I6).

### 3.3 Enable decision (evaluated at `configure`, in this order) [V]

```
otel_enabled requested?            no  → JSON mode
hook supported for OTel?           no  → warn "otel_enabled is only supported with the <hooks> hook, not <hook>; <fallback outcome>" → JSON mode
credential present?
  (BUILDKITE_TESTS_OTLP_TOKEN, else the JSON token)
                                   no  → silently JSON mode (matches JSON path, which also skips upload without a token; do not nag local devs)
otherwise                              → store options; actual SDK setup is DEFERRED (see §10.1)
```

`<fallback outcome>` = `"uploading results as JSON instead"` if the JSON token is set, else `"results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set"`. [V] `json_fallback_outcome`. Note bktec's relay token is useless to the JSON path, so the second message is the honest one in that case.

---

## 4. Wire contract (what goes over HTTP)

All [V] from `Analytics::API::TracesController` and the Ruby exporter config.

| Item | Value |
|---|---|
| Method / path | `POST <endpoint>` — default `https://tests-otlp.buildkite.com/v1/traces` |
| Body | OTLP/HTTP `ExportTraceServiceRequest`, protobuf binary. `Content-Type: application/x-protobuf`. JSON OTLP is **not** accepted. |
| Compression | `Content-Encoding: gzip`, pinned (never from env). Receiver accepts `gzip` or none. |
| Raw body limit | ≤ 900 KiB compressed-on-the-wire → else 413. Decoded ≤ 8 MiB. ≤ 50,000 spans per request. ≤ 10,000 synthesized executions per request (rest dropped). |
| `Authorization` | `Token token="<credential>"`. Receiver also accepts `Bearer`. Credential = suite API token or an OIDC token with `write_uploads`. Header omitted entirely if no token (Ruby); the receiver then returns 401/403. |
| `Buildkite-Tests-Run-Key` | The run key. Must match `/\A[!-~]{1,255}\z/` (1–255 printable ASCII, no spaces). Receiver returns 400 otherwise. Collector validates **before** loading the SDK and falls back to JSON with a warning (§11). The receiver derives the run from **this header**; the synthesizer separately requires `buildkite.run_key` on the span/resource for run metadata and does not cross-check the two. Send the same key in both places. |
| `Buildkite-Tests-Span-Stream` | `test` or `child`. One value per request, per processor. Older collectors send none; the intended rule is "absent = test". ⚠️ Receiver on main does not yet act on this header (TE-7322). bktec's relay forwards it as of test-engine-client PR #698 (draft, unmerged Oct 17 2026); released bktec versions drop it, so child spans sent through an older relay arrive unlabelled (see §15). |
| Response | Empty `ExportTraceServiceResponse` on 2xx. 200 means Kafka accepted the bytes, not that synthesis succeeded. `partialSuccess` is not populated and Ruby 2.17 does not read it. |
| Error semantics | 400 run key, 401/403 auth or org not flagged (`Feature::TestEngineOTLPTraceIngest`), 413 too large, 415 unsupported content type/encoding, 429 rate limit, 503 Kafka unavailable. |
| Retry (Ruby exporter 0.35.x, [V]) | Up to 5 retries after the first attempt (6 attempts). Retries on 408, 429, 502, 503, 504, redirects (against the original destination), and on open/read timeout, SSL error, socket error, system-call error, EOF, zlib error. Honours `Retry-After` (integer seconds or HTTP date) on 429 and 503; otherwise randomised exponential backoff. **Does not retry 400, 401, 403, 404, 413, 415, 500 or other 4xx/5xx** — those fail the batch immediately and are counted with reason = HTTP status. Each retry carries the same stream header. Retry sleeps are not clamped to the 30 s budget, so a flush can overrun it. Known departures [V upstream source — Go v1.47.0, JS and Python `main` as of Oct 17 2026; **re-verify at the version you pin**]: these exporters retry 429/502/503/504 with `Retry-After` support but **none retries 408**; Go's retry is duration-bounded (default initial 5 s, max 30 s, max elapsed 1 min), JS does up to 5 retries (1 s × 1.5, 5 s cap, ±20 %), Python `main` 6 attempts (2^n, ±20 %) bounded by the export timeout. Older Python releases (≤ 1.36) retried **408 and every 5xx** (including 500) with no `Retry-After` handling — unacceptable against the hard rule, so pin a newer exporter or wrap retries yourself. Accepting these differences from Ruby is [P] (Matt); the hard rule stands: never retry 400/401/403/404/413/415 or 500. |
| TLS | Verify peer with the system cert store. No client certs. |
| Export timeout | 30 s per export call (supplied by the batch processor). |

---

## 5. Resource (private provider and collector-managed global provider) [V] `producer_resource`

Precedence (later wins): SDK defaults and process attributes → `OTEL_RESOURCE_ATTRIBUTES` → `OTEL_SERVICE_NAME` (all three via `Resource.default`) → the collector's attributes below, each only if non-nil. A **suite-owned** global provider keeps its own resource; only the private provider is guaranteed to carry this one.

| Attribute | Source |
|---|---|
| `service.name` | `BUILDKITE_TEST_ENGINE_SUITE_SLUG` |
| `service.namespace` | `BUILDKITE_ORGANIZATION_SLUG` |
| `cicd.pipeline.run.id` | by detected CI (`run_env["CI"]`): Buildkite `BUILDKITE_BUILD_ID`; GitHub `GITHUB_RUN_ID`; CircleCI `CIRCLE_WORKFLOW_ID`; Codeship `CI_BUILD_ID`; else nil |
| `cicd.pipeline.run.url.full` | Buildkite `BUILDKITE_BUILD_URL`; GitHub `https://github.com/<GITHUB_REPOSITORY>/actions/runs/<GITHUB_RUN_ID>`; others nil. Only set when the run id is set. |
| `cicd.worker.id` | `BUILDKITE_AGENT_ID`, omitted if nil or whitespace-only; otherwise emitted **untrimmed** |
| `process.runtime.version` | final `run_env["language_version"]` (normally `RUBY_VERSION`; overridable via `env:`) |
| `vcs.ref.head.name` | final `run_env["branch"]` (CI branch, `BUILDKITE_ANALYTICS_BRANCH`, or `env:`) |
| `vcs.ref.head.revision` | final `run_env["commit_sha"]` |
| `vcs.ref.type` | `"tag"` if `BUILDKITE_TAG` non-empty else `"branch"`; only when branch is set |

The server reads from the resource: `vcs.ref.head.name` → run_env branch, `vcs.ref.head.revision` → commit_sha, `cicd.pipeline.run.id` (else `buildkite.build_id`) → build_id, `cicd.pipeline.run.url.full` → url (fallback), and any `buildkite.tag.*` on the resource → **upload-level** tags (and they also participate in execution tags, with span tags taking precedence). [V] `runEnvFrom`. Ruby does not put `buildkite.tag.*` on the resource itself, but a customer's `OTEL_RESOURCE_ATTRIBUTES=buildkite.tag.x=y` would reach it via `Resource.default` and become an upload tag. Configure-level tags live on test spans (§6.4).

Why the split: the resource identifies the *producer* (suite, pipeline run, worker, VCS ref) and is attached to every child span too; Test Engine run metadata is per-execution and lives on the test span only, so child spans stay small. [V] comment in `configure!`.

---

## 6. The test span (`test.execution`)

### 6.1 Shape [V]

| Property | Value |
|---|---|
| Name | `test.execution` |
| Kind | `INTERNAL` |
| Parent | none — started with an **empty** context so it is a trace root even inside an outer span |
| Links | one link to the Agent job span if `TRACEPARENT`/`TRACESTATE` extract to a valid W3C context; else none. Malformed values → no link, no warning. |
| Tracer (instrumentation scope) | name `buildkite-test-collector`, version = collector version. ⚠️ The UI classifies scope with `OtelSpan::SCOPE_CATEGORIES` (`/test/i` → TEST); keep this name. |
| Provider | the private test-span provider (§9.1) |
| Start time | when the collector's around-test wrapper starts, before framework setup hooks |
| End time | captured by the wrapper immediately after the test body + hooks return (realtime clock, not a frozen/mocked clock), then applied when the span is finished later by the reporter. If the captured end precedes the span start (clock step), fall back to the SDK's clock. |

### 6.2 Attribute ordering and the two-phase write [V] `start_test_span`, `finish_test_span`

The Ruby SDK keeps the **earliest** attributes when a span hits its count limit (128). Order matters:

**At start** (`start_test_span`):
1. `buildkite.execution.via` = `"otlp"`
2. `buildkite.run_key`
3. `test.case.result.status` = `"unset"` (placeholder, reserves the slot; replaced at finish)
4. the test's own attributes (§6.3) **excluding** `buildkite.tag.*`

**At finish** (`finish_test_span(span, test, end_timestamp)`, called from the reporter after the framework has settled the result; no-op when span is nil):
1. `record_result`: the test object reports `"passed"`, `"failed"` or `"skipped"` (RSpec `:pending` → `"skipped"`; anything else → nil). Map to `pass|fail|skipped` and set `test.case.result.status`; a nil/unmapped result **leaves the `"unset"` placeholder in place** (the server will then skip the execution — see I2, so map every framework outcome). Only `"failed"` also sets span status `ERROR` with description = failure reason truncated to 1,024 chars and adds one `exception` event per failure (§6.5); pass/skipped do not reset a status or clear events.
2. `describe_test`: recompute all attributes and `set_attribute` each, **including** ones already written at start (same values; this is a re-set, not a conflict). Order: test attrs (non-tag) → run attrs (non-tag) → tags. On key collision, run attrs override test attrs for non-tag keys; per-test tags override run-level tags. Nil test attrs are omitted.
3. `finish_span` with the captured end timestamp (fallback to SDK clock if it precedes start).

Each step rescues and warns independently so a half-described span still gets finished ("a half-described execution beats a missing one"). Finishing is *attempted*; an exception inside `span.finish` itself leaves the span unfinished and warned.

If `start_test_span` itself fails, the test still runs, no span exists (`nil` is returned and `with_test_span(nil)` just yields), and the failure is counted as one dropped test span → "TEST RESULTS MISSING" warning (§11). [V] `TestSpanMetricsReporter#record_start_failure`. `start_test_span` also returns nil when OTel is not enabled.

**Context while the test runs** [V] `with_test_span(span)`: sets context values `buildkite.test.execution` → the test span's trace_id and `buildkite.test.execution.span` → the span object, makes the span current, yields, restores the previous context afterwards. It has **no rescue**: exceptions from the test propagate. `current_test_span` returns the context's span only if OTel is enabled and the span is still recording; otherwise nil (lookup errors warn and return nil).

### 6.3 Test attributes [V] `Trace#otel_attributes`

| Attribute | Type | Value | Read by server? |
|---|---|---|---|
| `buildkite.test.scope` | string | same as JSON `scope` (RSpec: example group `full_description`) | **yes — test identity** |
| `buildkite.test.name` | string | same as JSON `name` (RSpec: example `description`) | **yes — test identity**; must be non-empty |
| `test.case.name` | string | full description (scope + name as the framework renders it) | no (semconv courtesy; UI may use) |
| `test.suite.name` | string | = scope | no |
| `code.file.path` | string | same as JSON `file_name`, with `location_prefix` prepended. RSpec: file from `example.id`, or the shared-example call site. | yes → location `path[:line[:col]]` |
| `code.line.number` | int | line of the example (shared examples: call-site line) | yes |
| `buildkite.test.execution.external_id` | string | UUIDv7 generated per execution (same value the JSON path sends as `external_id`) | **no** ⚠️ synthesizer never reads it; harmless to send |
| `buildkite.tag.<key>` | string | per-test tags set with `tag_execution` during the test (§3.1) | yes → execution tags |

The server also accepts `buildkite.test.location` as an explicit `path[:line[:col]]` override and `code.column.number`; Ruby sends neither. Line/column must be OTLP integers, not numeric strings.

Encoding: the test attributes above and per-test tag values are invalid-UTF-8-scrubbed (`strip_invalid_utf8_chars`, replacement-char encoding). Configure-level tag values are only `to_s`'d; run/resource values are passed through as-is. Nil values are omitted, never sent as empty strings.

Not carried by the OTel path (JSON-only): `execution_name_prefix`/`_suffix`, arbitrary custom `env:` keys, `history` (the legacy span tree). Do not invent attributes for them.

### 6.4 Run attributes (on every test span) [V] `run_attributes`

| Attribute | Source | Server reads it as |
|---|---|---|
| `buildkite.run_key` | CI env `key` | run_env.key (required) |
| `buildkite.build_number` | CI env `number` | run_env.number |
| `buildkite.job_id` | CI env `job_id` | run_env.job_id (header job id backfills if absent) |
| `buildkite.message` | CI env `message` | run_env.message |
| `buildkite.step_id` | `BUILDKITE_STEP_ID` | run_env.step_id |
| `buildkite.collector.name` | legacy run_env `collector` (Ruby: `ruby-buildkite-test_collector`) | run_env.collector — **keys the primary-selector registry** |
| `buildkite.collector.version` | collector version | run_env.version |
| `buildkite.location_prefix` | configured prefix | stripped back off the selector |
| `buildkite.test.framework.name` | the configured hook / `test_runner` property (Ruby: `rspec`), **not** an `env:`-overridden `run_env["test_runner"]` | run_env.test_runner — **keys the primary-selector registry** |
| `buildkite.test.framework.version` | framework version (`RSpec::Core::Version::STRING`) | not read |
| `buildkite.run_url` | final `run_env["url"]`, **only if it differs** from the native CI URL computed by `ci_pipeline_run` (the same value that becomes `cicd.pipeline.run.url.full`) | run_env.url (span wins over resource) |
| `buildkite.tag.<key>` | configure-level `tags` + automatic `ci.worker.id` = `BUILDKITE_AGENT_ID` (untrimmed; omitted if blank; explicit `tags:` entry with the same key wins) | execution tags |

Server truncates every run_env value to 1,024 chars and takes the first **non-blank** candidate in each fallback chain (span attr, then resource attr). Primary selector is derived only for registered `(collector, test_runner)` pairs: `ruby-buildkite-test_collector`/`rspec`|`cucumber`, `js-buildkite-test-collector`/`jest`|`playwright`|`cypress`, `python-buildkite-test-collector`/`pytest`. [V] `JsonPrimarySelector.DERIVERS`. Send exactly the legacy strings. ⚠️ A Go collector has no registry entry; selectors will be empty until ta-ingestion adds one.

### 6.5 Result and failure mapping [V] `record_result`, `Trace#otel_exception_events`

| Framework result | `test.case.result.status` | span status |
|---|---|---|
| passed | `pass` | UNSET |
| failed | `fail` | `ERROR`, description = failure reason[0,1024] |
| skipped / pending | `skipped` | UNSET |

Never emit any other value; the server drops the execution if the status is unrecognised. Map deliberately in each target (e.g. pytest `xfail` → `skipped`? decide and document; Jest `todo` → `skipped`).

Failure detail = one `exception` span event per individual failure (a test can have several: multiple expectations, failing after hooks):

| Event attribute | Value |
|---|---|
| `exception.message` | the failure's expanded message lines joined with `\n`; omitted if empty; capped at 10,243 chars (10,240 + `...`) |
| `exception.stacktrace` | backtrace lines joined with `\n`; omitted if empty; subject to the event attribute length limit (16,384 chars) |

An event with neither attribute is not added. Server mapping [V] `failureReason`, `failureExpanded`, `TestResult.truncatedFailureExpanded`:
- `failure_reason` = span status description, only when status is `ERROR` and the description is non-empty; truncated to 1,024 code points.
- Each `exception` event → one `failure_expanded` element: `expanded` = a one-element list holding the message as a single string (not split on newlines; empty/missing message falls back to `exception.type`), `backtrace` = stacktrace bounded first, then split on line breaks.
- Budgets are **character** counts, shared across the failures of one execution and consumed in event order: 10,240 message chars per execution, 102,400 backtrace chars per execution; per request 1,024,000 backtrace chars and 10,000 backtrace lines. Up to 100 exception events are considered per span; fewer may survive the budgets.

### 6.6 Annotations [V] `OTel.annotate`

`Buildkite::TestCollector.annotate(content)` in OTel mode adds event `test.annotation` with attribute `buildkite.annotation = content.to_s` to the **test span** (looked up from a context key, not the current span, because during the body the current span is the `test.body` phase). No-op when there is no recording test span in context. The public `annotate` routes to OTel only when OTel is on; when off it goes to the legacy tracer as before — never both.

### 6.7 Tests that never enter the wrapper [V] `Reporter#handle_example`

A test can fail without the around-test wrapper running (RSpec: failing `before(:context)`, an outer `around` that raises first). In OTel mode the reporter then starts a span at report time (so it times only the reporting), finishes it with the failure, and emits no phase spans. Tests skipped before running (`skip` metadata, filtered out) produce **no** span — same as the JSON path which has no trace for them.

### 6.8 Tags end-to-end [V] `worker_id_tag`, `tag_attributes`, `Trace#otel_attributes`, `test_span_attributes`; server `Tag.firstN`, `Tag.isValidValue`

Every tag is a `buildkite.tag.<key>` **string** attribute. There is no other tag encoding.

| Source | Set where | Lands on | Value handling | Mode |
|---|---|---|---|---|
| Automatic `ci.worker.id` | `configure`: `tags = worker_id_tag.merge(tags)` | test span (run attrs) | `BUILDKITE_AGENT_ID` untrimmed; omitted if nil/blank; an explicit `tags:` entry with the same key **wins** (merge order) | both JSON and OTel (same `tags` hash). JS and Python JSON paths already add this tag [V] (`uploadTestResults`, `run_env.py`); reuse it. Go has to add it. |
| Configure-level `tags:` | `configure(tags: {...})` | test span (run attrs, via `tag_attributes`) | key interpolated as-is, value `to_s`; **not** UTF-8-scrubbed | both |
| Per-test `tag_execution(key, value)` | inside a running test | test span (test attrs, via `otel_attributes`) | both must be strings (else `ArgumentError`); value UTF-8-scrubbed; raises outside a test | both |
| Customer resource tags | `OTEL_RESOURCE_ATTRIBUTES=buildkite.tag.x=y` (reaches `Resource.default`) | resource | passed through by the SDK | OTel only; Ruby never sets these itself |

Precedence on the **test span** (one key, several sources): per-test > configure-level > automatic. Implemented as: `tag_execution` tags are in `test_attributes`, configure+automatic in `run_attributes`, and `test_span_attributes` merges `run_attributes.merge(test_attributes).select(tag)` so test tags overwrite. Port the same three-way merge; do not let a per-test tag be overwritten by a run tag.

Precedence on the **server**: resource `buildkite.tag.*` → upload-level tags *and* a base layer for execution tags; span `buildkite.tag.*` overrides per key → execution tags. Then limits: key ≤ 64 B, value non-blank and ≤ 128 B, scalar values only; invalid entries **dropped** (not truncated); when more than 10 survive, the first 10 in **lexicographic key order** are kept (`Tag.firstN` sorts by key — send order is irrelevant). `test.selector.primary` / `test.selector.id` overrides have a 512 B value limit instead.

Consequences for the port:
- A collector that sends 11 tags silently loses the lexicographically last one. Do not pad with automatic tags beyond `ci.worker.id`.
- Keys are never transformed (no lowercasing, no `-`→`_`); what the customer writes is the key.
- Configure-level tag values are stringified by the collector when building attributes (`to_s`/`String()`/`str()`); `tag_execution` keeps rejecting non-string arguments (§3.1) — do not coerce there. The server drops non-scalar values and SDKs may drop non-primitive attribute values before export, so never send anything but strings.
- `annotate(content)` is **not** a tag; it is a `test.annotation` event (§6.6).

### 6.9 Worked example — one failing test, rendered readably (illustrative, not byte-exact)

Request to `POST https://tests-otlp.buildkite.com/v1/traces`, stream `test`:

```
Content-Type: application/x-protobuf
Content-Encoding: gzip
Authorization: Token token="bkt_..."
Buildkite-Tests-Run-Key: 0192d1a2-...-build-42-job-7
Buildkite-Tests-Span-Stream: test

resource.attributes:
  service.name            = "my-suite"                     # BUILDKITE_TEST_ENGINE_SUITE_SLUG
  service.namespace       = "my-org"                       # BUILDKITE_ORGANIZATION_SLUG
  cicd.pipeline.run.id    = "0192d1a2-..."                 # BUILDKITE_BUILD_ID
  cicd.pipeline.run.url.full = "https://buildkite.com/my-org/my-pipeline/builds/42"
  cicd.worker.id          = "agent-123"                    # BUILDKITE_AGENT_ID
  process.runtime.version = "3.3.4"
  vcs.ref.head.name       = "main"
  vcs.ref.head.revision   = "9f3c..."
  vcs.ref.type            = "branch"
  (+ whatever the SDK's Resource.default / OTEL_RESOURCE_ATTRIBUTES added)

scope_spans[0].scope: name = "buildkite-test-collector", version = "2.17.0"

span:
  name            = "test.execution"
  kind            = INTERNAL
  trace_id        = <16 CSPRNG bytes>      span_id = <8 CSPRNG bytes>
  parent_span_id  = <empty>                 # root — the server's candidate rule
  links           = [ {trace_id/span_id from TRACEPARENT} ]   # only if present and valid
  start_time      = wrapper start (Unix ns)  end_time = captured realtime end (Unix ns, ≥ start)
  status          = ERROR, message = "expected 2, got 3"      # failure_reason[0,1024]
  attributes (FINAL exported state — not API call order; §6.2 writes via/run_key/status/test attrs at start,
   then re-sets test+run attrs and replaces the status at finish; Python must additionally re-set the
   synthesis keys last, §14.1):
    buildkite.execution.via           = "otlp"
    buildkite.run_key                 = "0192d1a2-...-build-42-job-7"
    test.case.result.status           = "fail"                 # was "unset" at start
    buildkite.test.scope              = "Calculator#add"
    buildkite.test.name               = "adds two numbers"
    test.case.name                    = "Calculator#add adds two numbers"
    test.suite.name                   = "Calculator#add"
    code.file.path                    = "./spec/calculator_spec.rb"
    code.line.number                  = 12                     # int, not "12"
    buildkite.test.execution.external_id = "0192d1b0-..."      # UUIDv7; server ignores
    buildkite.build_number            = "42"
    buildkite.job_id                  = "0192d1a3-..."
    buildkite.message                 = "Fix rounding"
    buildkite.step_id                 = "0192d1a2-...-step"
    buildkite.collector.name          = "ruby-buildkite-test_collector"
    buildkite.collector.version       = "2.17.0"
    buildkite.test.framework.name     = "rspec"
    buildkite.test.framework.version  = "3.13.0"
    buildkite.tag.ci.worker.id        = "agent-123"            # automatic
    buildkite.tag.team                = "payments"             # configure-level tags:
    buildkite.tag.flaky               = "true"                 # tag_execution in the test
  events:
    { name = "exception",
      attributes = { exception.message = "expected 2, got 3\n\n  Diff: ...",
                     exception.stacktrace = "./spec/calculator_spec.rb:14:in ...\n..." } }
    { name = "test.annotation", attributes = { buildkite.annotation = "retried once" } }  # if annotate() was called
```

What the server does with it: candidate (root + `via=otlp`) → test id from (the **authenticated suite id** from the token — `service.name` does not select the suite, `Calculator#add`, `adds two numbers`) → location `./spec/calculator_spec.rb:12` → result `failed`, `failure_reason` = status message, `failure_expanded[0]` = {expanded: [message], backtrace: lines} → run_env from the `buildkite.*` attrs plus branch/commit/build_id from the resource → execution tags `ci.worker.id`, `team`, `flaky`. A passing test looks the same with status `UNSET`, `test.case.result.status = "pass"`, no `exception` event. A skipped test: `"skipped"`, status `UNSET`.

A second request on stream `child` would carry `test.body` (parent = the span above, status `ERROR`, one `exception` event) and any instrumentation spans under it; `test.setup`/`test.teardown` are absent because they grouped nothing and stayed `UNSET`.

---

## 7. Phase spans [V] `PhaseSpans`, `start_phase_span`, `finish_phase_span`

Three fixed child spans of the test span that group instrumentation by when it ran. The UI expands exactly one level under the test span and stops, so the names are a contract:

| Phase | Name | Wraps |
|---|---|---|
| setup | `test.setup` | framework before-hooks (RSpec `run_before_example`) |
| body | `test.body` | the test body itself |
| teardown | `test.teardown` | framework after-hooks (RSpec `run_after_example`) |

Rules:
- Kind `INTERNAL`; tracer from the **global** provider (same as instrumentation spans), scope name `buildkite-test-collector`.
- Parent = the **test span explicitly**, not the current span (an around hook's open span must not capture a phase). Each phase is made current for its duration (attach, then detach **before** the phase is finished) so instrumentation nests under it.
- Up to three phases are *attempted*: setup always; body only if setup returned normally (a setup exception or skip leaves no body span); teardown always (wraps after-hooks and mock verification), finishing the body span first.
- Failures in a phase → `record_exception` event per failure and status `ERROR` with the last message[0,1024]. A "skip declared in example" exception is not a failure. Teardown records only failures newly observed during teardown (the example's failure list *after* teardown minus the list *before*, in order — e.g. before `[body_err]`, after `[body_err, after_hook_err]` → teardown records `[after_hook_err]`), so a body failure is never re-attributed to teardown. The body span records every failure present when it finishes.
- A fresh set of phases per run of the same example (retries). Body-span state is cleared after finishing.
- Export filter: the `ChildSpanForwarder` exports a phase **only if it grouped ≥1 tracked child span or its status ≠ UNSET** (so a failed empty phase is exported). A passing test with no instrumentation produces zero child-stream spans. Phases bypass the customer `otel_span_filter`. A suite-owned provider's own exporters still receive every phase span, empty or not — the rule applies only to Buildkite's child stream.
- No phases when OTel is off or when no recording test span is in context.

---

## 8. Child spans: forwarder, filter, BYO provider, sampling

### 8.1 Which spans are "child spans" [V] `ChildSpanForwarder`

Any span started on the global provider whose **parent context** carries the collector's context key (set by `with_test_span`) **and** whose `trace_id` equals the test span's. Spans started outside a test, or in a different trace (e.g. a customer's own root span), are ignored.

`with_test_span` sets two context values while the test runs: `buildkite.test.execution` → test span trace_id (the forwarder's key) and `buildkite.test.execution.span` → the span object (used by `current_test_span` and `annotate`), and makes the test span current.

### 8.2 Forwarder behaviour [V]

- Registered as a span processor on the global provider. `on_start` records matching spans (under a mutex); `on_finish` hands accepted spans to the child batch processor.
- **Phase recognition is by name only**: a child whose name is `test.setup` / `test.body` / `test.teardown` is treated as a phase (no scope or attribute check). `on_start` of a tracked non-phase child whose **immediate parent** is a tracked phase marks that phase "populated". This happens before the filter runs, so a drop-everything filter still leaves the phase populated and exported. A phase is exported iff populated or status ≠ UNSET (§7).
- Finishing a span removes its tracking state; a child that starts after its parent phase finished cannot re-populate the phase.
- Children are forwarded **on their own completion**, not held until the test ends, and are never stamped with the test's result (D110).
- With a filter: `accept` runs under the lock, the filter runs **outside** it on the thread that finished the span (so it may run concurrently on several threads), then `@active` is re-checked before enqueueing. Without a filter: accept + enqueue under one lock.
- `force_flush` delegates to the child processor when active; an inactive forwarder returns SUCCESS without calling it. `shutdown` only makes the forwarder inert (clears tracking) — it does **not** shut down the child processor (the collector's `shutdown` does that). After shutdown, spans whose filter was mid-flight are dropped.
- Child processor failures (queue overflow, export failure) never affect the test span or the suite.
- Dropping a span via the filter does not re-parent its children; they still reference the dropped parent id.

### 8.3 Customer filter [V] `SpanFilter`

- `otel_span_filter.call(span) → truthy keeps, falsy drops`. Applies to child spans only, never test spans, never phase spans.
- Any exception in the filter → retain the span, warn **once per filter instance** (`Could not filter OpenTelemetry child span, retaining it: … Further filter failures will not be reported.`), keep going. "Once" is best-effort under concurrency (two threads failing simultaneously may both warn). A non-callable filter raises `ArgumentError` at configure time (I7).
- Re-entrancy guard: a span finished *by* the filter is retained without calling the filter again. The guard is a thread-wide flag (covers fibers on that thread), so a filter that finishes several spans retains all of them.

### 8.4 Global provider: collector-managed vs suite-owned [V] `configure_child_export`

```
global provider is still the SDK proxy (nobody configured OTel)?
  yes → collector-managed: OpenTelemetry::SDK.configure { resource; id_generator = CSPRNG; add_span_processor(forwarder); use_all }
        (use_all = install every registered instrumentation gem)
        then verify a real provider was installed, else raise
  no  → suite-owned: provider.add_span_processor(forwarder); leave resource, sampler, instrumentation alone
        provider must respond to add_span_processor, else raise
raise/any failure → warn "OpenTelemetry child span export disabled: …; test.execution export remains enabled"; test spans still work
```

Consequences to preserve:
- A suite-owned provider's **sampler** applies to child spans. `OTEL_TRACES_SAMPLER=traceidratio` drops children but never test spans (private provider is always-on). `parentbased_*` variants inherit the test span's sampled flag (always-on root) → effectively always-on for children.
- If the suite shuts down its own provider, private test-span export keeps working.
- Setup is **deferred** until the framework's suite-start hook so the application's own OTel configuration (loaded by spec helpers / app boot) wins. [V] `otel_setup_spec`.
- The detect-then-attach shape above is Ruby's. It does not port one-to-one: JS 2.x has no runtime `add_span_processor`, and Go cannot always tell "unconfigured" from "custom provider". §14.0 gives the per-SDK rule; the invariant that survives is **never replace or reconfigure a provider the suite installed, and never let child-path trouble disable test-span export**.

### 8.5 Sampling (planning context)

Head sampling of child spans is the customer's choice via the standard SDK sampler env on the global provider; the collector adds no sampler of its own. Test spans are never sampled out. (D-rows in `local/otel/decisions.md` in the `buildkite/buildkite` repo; the public post is `post-sampling.md` there.)

---

## 9. Export pipeline

### 9.1 Two providers, two processors, two exporters [V] `build_test_span_provider`, `configure_child_export`, `batch_processor`

| | Test stream | Child stream |
|---|---|---|
| Provider | private `TracerProvider` | global provider |
| Sampler | `ALWAYS_ON` | collector-managed: SDK default (parentbased_always_on); suite-owned: theirs |
| ID generator | CSPRNG (`SecureRandomIdGenerator`, rejects all-zero) | same when collector-managed |
| Resource | §5 | same when collector-managed |
| Span limits | attr count 128, attr length 10,243 chars, event attr count 128, event attr length 16,384, event count 100; link limits SDK default | SDK defaults / env |
| Batch processor | queue 8,192, batch 120, schedule delay 1,000 ms, export timeout 30 s | queue 2,048, batch 512, delay 5,000 ms, export timeout 30 s |
| Env overrides | `BUILDKITE_TESTS_OTEL_TEST_SPAN_{QUEUE,BATCH}_SIZE` | `BUILDKITE_TESTS_OTEL_CHILD_SPAN_{QUEUE,BATCH}_SIZE` |
| Exporter headers | `Authorization`, `Buildkite-Tests-Run-Key`, `Buildkite-Tests-Span-Stream: test` | …`: child` |
| Metrics reporter | `TestSpanMetricsReporter` | `ChildSpanMetricsReporter` |

Why batch 120 on the test stream: a failure at both message and stack-trace caps gzips to ≈4.2 KiB; 120 × 4.2 KiB stays under the 900 KiB request cap. Typical spans are ≈0.3 KiB. [V] comment on `TEST_SPAN_MAX_EXPORT_BATCH_SIZE`. This is headroom, not a guarantee — there is no size-based splitter, so a batch of 120 spans with 128 max-length attributes each could still exceed 900 KiB and be rejected whole. Size your batch so worst-case × batch < 900 KiB compressed and accept the residual risk, or add a size splitter if the target SDK makes that cheap.

Ruby SDK `BatchSpanProcessor` behaviour, for reference [V]. The portable guarantees are only: every finished test span is either exported or **counted as dropped and warned**; nothing blocks the test thread; flush/shutdown are bounded by the shared 30 s deadline (§10.2). The mechanics below are Ruby-incidental (Python's flush, for example, drains every batch and ignores exporter results — see §14.0):
- Both worker threads start at setup, not lazily.
- Queue full → the **oldest** queued spans are dropped to admit the new one (reason `buffer-full`). Accepted departure [V upstream source, Oct 17 2026]: Python's `BatchSpanProcessor` also evicts the oldest, but Go's and JS's drop the **new** span. Either is acceptable; what must hold is that every drop is counted and surfaced (§9.4, §11). Do not reimplement the queue to match Ruby.
- A batch whose export fails after the exporter's own retries (§4) is dropped (reason `export-failure`); nothing re-queues it.
- `force_flush` exports batch by batch and **stops at the first failed batch**, requeueing the remainder (they may export later or be dropped at shutdown).
- `shutdown` drains what it can within the deadline; leftovers are reported as dropped with reason `terminating`.

Why span attribute length 10,243: ta-ingestion `Test.NAME_MAX_LENGTH` = 10,240; the SDK truncates to limit−3 + `...`, the server strips back to 10,240 before deriving the test ID, so a long name keeps the JSON identity. Keep this exact number.

### 9.2 Exporter hardening [V] `ExporterGuard`

The SDK's batch worker only rescues `StandardError`; a non-fatal `Exception` (e.g. a WebMock `NetConnectNotAllowedError`) would kill the worker thread. Wrap `export`: rescue non-fatal → count `otel.otlp_exporter.failure` with reason = class name, warn **once per exception class** (`Could not export OpenTelemetry spans: <Class>. Further <Class> export failures will not be reported.`), return `FAILURE`. Re-raise fatal.

Port the idea, not the mechanism: in each target, find what the SDK's exporter/processor does *not* catch and make sure it cannot take the worker down or leak a stack trace into test output.

### 9.3 Test-tooling exemptions [V] `exempt_from_vcr`, `exempt_from_webmock`

Ruby registers a VCR `ignore_request` hook matching `POST` + endpoint **host + port + path** (scheme and query string are not compared) and extends WebMock's allow-list with the endpoint **hostname only** (a new list is built that preserves existing string/regex/proc entries) so HTTP-stubbing suites do not block export. Exemptions are applied only if the library is already loaded at setup time — a suite that requires VCR/WebMock *after* suite start is not covered. Failures here are warned (`Could not exempt the OTLP endpoint from VCR|WebMock: …`) and ignored. Each target should identify the equivalent (Python `responses`/`vcrpy`, JS `nock`/`msw`) and either exempt the endpoint or document that the customer must.

### 9.4 Metrics reporter semantics [V] `SpanMetricsReporter`

Hooks the SDK's processor/exporter metrics; `record_value`/`observe_value` are no-ops, only `add_to_counter` matters:
- `otel.bsp.dropped_spans` with reason `buffer-full`, `export-failure` or `terminating`.
- `otel.otlp_exporter.failure` with reason = HTTP status (e.g. `403`), an exception class name (via `ExporterGuard`), or one of the exporter's literal strings `timeout`, `socket_error`, `openssl_error`, `eof_error`, `zlib_error`. Remembered as "last failure".
- `otel.bsp.export.success` clears the remembered failure.

Reporting (Ruby; the immediate first-drop warning depends on an SDK hook Go/JS/Python lack — see §14.0 for the accepted departure): on the first drop in a suite run warn immediately, naming the reason; **only `export-failure` drops** append the remembered cause (`last OTLP failure: <status|class>`). At `force_flush`/`shutdown`, warn the run total if it exceeds what was already warned. Reporting totals resets the drop counts (so a warm process gets fresh accounting per suite run) but **not** the remembered failure. `ExporterGuard`'s once-per-class suppression is shared across both exporters and reset at shutdown; filter-warning suppression is per filter instance.

---

## 10. Lifecycle

### 10.1 Sequence [V]

```diagram
┌─────────────────────────┐
│ configure(otel_enabled) │  validate options; decide enable (§3.3); STORE options; hook framework
└───────────┬─────────────┘
            ▼
┌─────────────────────────┐
│ suite start hook        │  start_otel → OTel.configure!:
│ (RSpec before(:suite))  │    validate run key → require SDK → VCR/WebMock exemptions (self-rescuing) →
│                         │    resource, run attrs → private provider+BSP (test)   [mandatory]
│                         │      ↳ any exception escaping up to here → warn + JSON fallback (this process)
│                         │    → global provider+forwarder+BSP (child)             [optional]
│                         │      ↳ failure → warn, keep OTel mode with test spans only
│                         │    → register at_exit shutdown (once)
│                         │  then register the result reporter/formatter
└───────────┬─────────────┘
            ▼  per test
┌─────────────────────────┐
│ around-test wrapper     │  trace = Trace.new(example, external_id: uuid7)
│                         │  span = start_test_span(trace)       (root, via, run_key, status=unset, test attrs)
│                         │  with_test_span(span) { run }        (context key + current span)
│                         │    ├ setup phase  → test.setup
│                         │    ├ body phase   → test.body
│                         │    └ teardown     → test.teardown
│                         │  ensure: capture end timestamp; stash span on trace
└───────────┬─────────────┘
            ▼
┌─────────────────────────┐
│ reporter (result known) │  failure_reason/expanded from framework → finish_test_span(span, trace, end_ts)
│                         │  no JSON queueing; forget the trace
└───────────┬─────────────┘
            ▼
┌─────────────────────────┐
│ suite end hook          │  OTel.force_flush  (30 s shared budget, test queue first, then child)
│ (RSpec after(:suite))   │  warn dropped totals; no artifact file in OTel mode
└───────────┬─────────────┘
            ▼
┌─────────────────────────┐
│ process exit (at_exit)  │  OTel.shutdown once: forwarder inert → shutdown test queue then child
│                         │  (same 30 s shared budget) → warn dropped totals → clear all state
└─────────────────────────┘
```

### 10.2 Rules [V]

- **Deferred setup**: never touch the OTel SDK in `configure`. Suite-start is the earliest safe point (the app's own OTel config has loaded). Spec: "sets up OpenTelemetry after application libraries have loaded", "starts setup after the application configures its provider".
- **Flush at suite end, shutdown at process exit**: suite hooks may run more than once per process (warm test pools). `force_flush` pushes finished spans without stopping export. `shutdown` is registered **once** on first successful configure and is a safe no-op when nothing is configured; the "registered" flag survives shutdown so a reconfigure cannot stack handlers.
- **Shared deadline**: flush/shutdown walk `[test queue, child queue]` against one 30 s monotonic deadline so an unreachable endpoint cannot block twice; test spans get the budget first; the first error is returned, every queue still gets a turn.
- **Shutdown order**: deactivate forwarder (so no new child spans enqueue) → shutdown test processor → shutdown child processor → warn totals → nil every ivar and reset `ExporterGuard`. After shutdown the global provider stays installed with the (now inert) forwarder still registered; the run identity is cleared so a later `configure!` can start a **new** run in the same process; the context keys and the "at_exit registered" flag survive.
- **Reconfigure in the same process** (`configure!` called again while configured): nil or unchanged run key → no warning. Different run key → `warn_run_mismatch` and keep the original run: `OpenTelemetry span export is already configured for run <old.inspect> and cannot switch to run <new.inspect>: results will still be attributed to the earlier run. Reporting a new run requires a new process.` In every case the `Authorization` header is then refreshed on both exporters in place (expiring OIDC tokens) — even after a mismatch. Missing/blank token → keep the current header. Refresh is best-effort: failure warns `Could not refresh the OTLP Authorization header; export continues with the previous token` (or the `: <Class>: <msg>` form when an exception is caught).
- **Partial setup failure**: if the private provider fails → stop its processor, raise → whole OTel off → JSON fallback. If only child export fails → stop child processor, make forwarder inert, warn, keep test-span export.

### 10.3 Component checklist, in build order

Each line is one unit the port must contain; the section tells you its contract. Build top to bottom. The first end-to-end milestone is steps 1–9 together (setup, wrapper, reporter finish, flush): only then is a span finished with a real status *and* delivered. Child export and phases are additive after that.

| # | Component | Contract | Done when |
|---|---|---|---|
| 1 | Config option `otel_enabled` (default off) + `otel_span_filter`; enable decision | §3.1, §3.3 | JSON mode is byte-identical with the flag off; unsupported framework / no credential behave per §3.3 |
| 2 | Run key validation + CI env resolution | §3.2, §4 (`Buildkite-Tests-Run-Key` regex) | invalid key warns and falls back **before** the SDK is imported |
| 3 | Lazy SDK import + dependency check | §1.2 | missing SDK → "disabled: <Class>: <msg>" + fallback, no crash |
| 3a | Suite-start orchestration: mandatory setup (steps 4–6) inside one rescue → on failure stop anything half-built and fall back; optional child setup (step 11) inside its own rescue; register exit shutdown once on success; **only after mandatory setup succeeds** disable the JSON upload/artifact/legacy tracer for this process | §10.1, §10.2, §1.1 | a provider that fails half-way leaves no running threads and the JSON path still runs; a child failure leaves test export on |
| 4 | Resource builder | §5 | attributes exactly as the table, nil omitted, `Resource.default` merged underneath |
| 5 | Run attributes builder | §6.4, §6.8 | legacy collector/test_runner strings; `run_url` only when it differs; tags merged with `ci.worker.id` |
| 6 | Private test-span provider: always-on sampler, CSPRNG id generator, span limits, BSP (8192/120/1 s/30 s), **one** exporter with explicit endpoint/headers/gzip/protocol/TLS, stream header `test` (the `child` exporter is created only in step 11) | §9.1, I5, I6, §14.0 | `OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_SPAN_*`/`OTEL_EVENT_*`, `OTEL_TRACES_SAMPLER` set to garbage have no effect on test-span export (`OTEL_RESOURCE_ATTRIBUTES` is deliberately still honoured, §5); limits pinned |
| 7 | Per-test wrapper: root span with start-order attributes, context key, make current (JS: with a real async context manager, §14.0), run, capture realtime end, stash span | §6.1–§6.3, §8.1 | span is a root, carries via/run_key/scope/name, `current_test_span` works across an `await`/thread |
| 8 | Reporter finish: result mapping, status + exception events (strings truncated by the collector), re-describe, finish with captured end; reporter-created spans for tests that never entered the wrapper | §6.5, §6.2, §6.7 | pass/fail/skipped all land; fail has status message + events; identity attrs survive overflow (Python: re-set last, §14.1); a before-all failure yields one span per affected test, no duplicates |
| 9 | Suite-end `force_flush` and process-exit `shutdown`, one shared 30 s deadline, registered once | §10.1, §10.2, §14.0 | **milestone:** one real run shows accepted `test.execution` spans in Test Engine with the JSON-identical test ids; flush test queue first then child; reconfigure after shutdown starts a fresh run |
| 10 | Drop/export accounting + warning catalogue | §9.4, §11, §14.0 | `TEST RESULTS MISSING` totals at flush/exit (immediately on first drop where the SDK allows); pending vs dropped kept apart at a deadline; child drops best-effort wording; exact phrases kept |
| 11 | Child path: global provider detect-or-install (per-SDK rules in §14.0), forwarder (`on_start`/`on_end`), customer filter, child BSP (2048/512/5 s/30 s), second exporter with stream `child`; shutdown deactivates the forwarder first | §8, §9.1, §10.2 | suite-owned provider left untouched; forwarder inert after shutdown; filter failure retains + warns once; child failure never touches test export |
| 12 | Phase hooks `test.setup` / `test.body` / `test.teardown` on the **global** provider, parent = test span explicitly | §7 | populated-or-non-UNSET export rule; teardown failures = after − before |
| 13 | Token refresh / run-key mismatch on reconfigure | §10.2, I8 | new token swapped on both exporters; different key warns and keeps original |
| 14 | HTTP-stub exemptions or documented customer action | §9.3 | the target's VCR/WebMock equivalents don't block export, or docs say what to do |
| 15 | `annotate` → `test.annotation` event; `tag_execution` → `buildkite.tag.*` | §6.6, §6.8 | routed to OTel only when on; no-op/raise semantics per §3.1 |
| 16 | Tests from §16 incl. the harness, docs (`opentelemetry.md` equivalent: dependency install, env vars, opt-in flag, fallback behaviour), CHANGELOG | §16, §15 | every §16 row fails against a plausible wrong implementation |

---

## 11. Fail-open rules and warning catalogue [V]

All warnings go to stderr with the prefix `[buildkite-test_collector]` (use the target collector's existing prefix). Table lists the Ruby wording; keep the meaning and the key phrases (`TEST RESULTS MISSING`, `results are unaffected`, `Further … will not be reported`) so operators can grep across collectors.

| Situation | Behaviour | Message (Ruby) |
|---|---|---|
| `otel_enabled` with unsupported hook | JSON | `otel_enabled is only supported with the rspec hook, not <hook>; <fallback>` |
| No credential | JSON, **silent** | — |
| Invalid run key | JSON, before SDK load | `OpenTelemetry span export disabled: run key <inspect> is invalid. Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); it must be 1-255 printable ASCII characters without spaces.` |
| SDK missing / exception escaping the **mandatory** test-span setup (requires, resource, private provider) | JSON (this process) | `OpenTelemetry span export disabled: <Class>: <msg>` then `otel_enabled is set, but OpenTelemetry could not be configured (see the warning above); <fallback>`. Child-export and VCR/WebMock failures never reach this row — they rescue internally (rows below). |
| Child export setup fails | test spans only | `OpenTelemetry child span export disabled: <Class>: <msg>; test.execution export remains enabled` |
| Reconfigure with different run key | keep original | `OpenTelemetry span export is already configured for run <old.inspect> and cannot switch to run <new.inspect>: results will still be attributed to the earlier run. Reporting a new run requires a new process.` |
| Token refresh yields no header / raises | keep previous token | `Could not refresh the OTLP Authorization header; export continues with the previous token` / `Could not refresh the OTLP Authorization header: <Class>: <msg>` |
| VCR / WebMock exemption fails | export still configured | `Could not exempt the OTLP endpoint from VCR: …` / `… from WebMock: …` |
| Bad `BUILDKITE_TESTS_OTEL_*_SIZE` | default | `<NAME> must be a positive integer up to 2147483647, …` (once per setup evaluation, §3.2) |
| Batch > queue | both defaults | `<BATCH_NAME> must be <= <QUEUE_NAME>; using defaults …` (once per setup evaluation) |
| `start_test_span` raises | test runs, counted as dropped | `TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (could not start span: <Class>: <msg>).` + `OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.` |
| Test span dropped by BSP | first drop immediate, totals at flush/exit | `TEST RESULTS MISSING: OpenTelemetry dropped N test.execution span(s) (<reason>[, last OTLP failure: <status|class>]).` + second line as above; total: `… dropped N test.execution span(s) so far this run; those test executions were not uploaded to Buildkite Test Engine.` |
| Child span dropped | same cadence | `OpenTelemetry dropped N child span(s) (<detail>); test.execution results are unaffected.` |
| Exporter raises non-StandardError | batch fails | `Could not export OpenTelemetry spans: <Class>. Further <Class> export failures will not be reported.` |
| Filter raises | retain | `Could not filter OpenTelemetry child span, retaining it: <Class>: <msg>. Further filter failures will not be reported.` |
| record_result / describe_test / finish raise | span still finishes (finishing is attempted regardless) | `Could not record the OpenTelemetry test result: …` / `Could not describe OpenTelemetry test span: …` / `Could not finish OpenTelemetry test span: …` |
| Reporter cannot finish the span at all (outer rescue) | continue | `Could not finish the OpenTelemetry test span: …` (note the extra "the" — distinct from the inner message) |
| Reporter cannot build failure text | span finishes without failure detail | `Could not describe test failure: …` |
| `current_test_span` / `annotate` raise | continue | `Could not read the current OpenTelemetry test span: …` / `Could not annotate OpenTelemetry test span: …` |
| Phase start / finish raise | test continues, phase missing | `Could not start OpenTelemetry <phase> span: …` / `Could not finish OpenTelemetry phase span: …` |
| Context attach / detach raise | test continues | `Could not make OpenTelemetry span current: …` / `Could not restore the OpenTelemetry context: …` |
| Forwarder `on_start` / `on_finish` / `force_flush` raise | child span lost, test unaffected | `Could not track OpenTelemetry child span: …` / `Could not export OpenTelemetry child span: …` / `Could not flush OpenTelemetry child spans: …` |
| flush / shutdown raise | continue | `Could not flush OpenTelemetry spans: …` / `Could not shut down OpenTelemetry span export: …` |

Fatal exceptions (process exit, signals, OOM) are always re-raised through every boundary.

---

## 12. Server contract summary (ta-ingestion `OtlpExecutionSynthesizer`) [V]

Read this as "what the server does with what you send".

- **Candidate**: root span (parent_span_id absent or empty) with `buildkite.execution.via = "otlp"`. If the current key is present at all it wins over the legacy `execution.via` even when its value is empty or wrong; the legacy key is consulted only when the current key is absent. Duplicate attribute keys → last occurrence wins. Everything else in the request is stored as a child span, never a result.
- **Validation** (fail → execution silently skipped, counted in a `skipped` metric): `buildkite.test.name` non-empty; status ∈ pass/fail/skip/skipped/pending; run key resolvable from span `buildkite.run_key` or resource `buildkite.run_key`; `0 < start ≤ end`.
- **Identity**: `deriveTestId(suiteId, scope, name)`, scope ≤ 1,024 chars, name ≤ 10,240 chars after truncation. Scope attr `buildkite.test.scope`.
- **Location**: `buildkite.test.location` if present, else `code.file.path` + `:code.line.number` + `:code.column.number`. `buildkite.location_prefix` is stripped back off when deriving the selector.
- **Result**: `test.case.result.status` → passed/failed/skipped/pending. `failure_reason` = span status message. `failure_expanded` from `exception` events (§6.5).
- **run_env**: span attrs win over resource attrs for `key,url,number,job_id,step_id,message,collector,version,test_runner,location_prefix`; `branch`, `commit_sha`, `build_id` come only from the resource (`vcs.ref.head.name`, `vcs.ref.head.revision`, `cicd.pipeline.run.id`/`buildkite.build_id`). `url` resolution order: span `buildkite.run_url` → resource `cicd.pipeline.run.url.full` → resource `buildkite.run_url`. Values truncated to 1,024. Job id from the authenticated envelope backfills a missing `job_id`.
- **Tags**: only `buildkite.tag.*`, scalar values only. Resource → upload tags; span → execution tags. Limits mirror JSON: 10 tags each, key ≤ 64 B, value ≤ 128 B. When more than 10 are sent, the **first 10 in lexicographic key order** are kept (not send order). Invalid keys and blank/oversized values are **dropped, not truncated**. Customer overrides of the derived selector tags (`test.selector.primary`, `test.selector.id`) have their own 512 B value limit.
- **Ignored** (sent by Ruby, never read): `buildkite.test.execution.external_id`, `test.case.name`, `test.suite.name`, `buildkite.test.framework.version`, span links, `process.runtime.version`, `cicd.worker.id`, `vcs.ref.type`. Sending them is fine and future-proof; do not rely on them.
- **Caps**: 10,000 executions/request (excess dropped, counted), 50,000 spans/request, 8 MiB decoded, 900 KiB wire.
- **Receiver gating**: org must have `Feature::TestEngineOTLPTraceIngest` (403 otherwise). Collectors must not retry 403.

---

## 13. Identity parity checklist (do this before anything else)

For the target collector, open its JSON upload code and record exactly how it computes, per test: `scope`, `name`, `location`, `file_name`, `identifier`, `result`, `failure_reason`, `failure_expanded`, `id` (external id), tags, and `run_env.collector` / `run_env.test_runner`. The OTel attributes must be computed by **the same functions** (share the code, do not re-implement), so that:

| JSON field | OTel attribute |
|---|---|
| `scope` | `buildkite.test.scope` (and `test.suite.name`) |
| `name` | `buildkite.test.name` |
| `file_name` (with prefix) | `code.file.path` |
| `location` line | `code.line.number` |
| `result` | `test.case.result.status` via §6.5 table |
| `failure_reason` | span status description |
| `failure_expanded[]` | `exception` events |
| `id` | `buildkite.test.execution.external_id` |
| `tags` | `buildkite.tag.*` |
| `run_env.*` | §6.4 |

Verify with one real run in both modes against the same suite and compare the test IDs in Test Engine (or compare the `(scope, name)` pairs from the JSON payload with the span attributes).

---

## 14. Per-target notes — [P] proposals, verify before building

Each target gets its own thread; these notes are the starting hypotheses, not findings. The implementer must confirm each against the framework's actual execution model and record what it found.

### 14.0 SDK mechanism map [V upstream SDK source: Go **v1.47.0**; JS `@opentelemetry/sdk-trace` 2.x / exporter `main`; Python `main` (post-1.36) — all read Oct 17 2026]

Which SDK knob implements each requirement. Ruby column is the reference. SDK behaviour is [V] **for those versions only**: pin the version in the port, re-read these rows against it, and keep the §16 isolation tests so a bump that changes behaviour fails loudly. Where this table says "accept the departure", that acceptance is [P] until Matt signs it.

| Requirement | Ruby (reference) | Go `go.opentelemetry.io/otel/sdk/trace` + `exporters/otlp/otlptrace/otlptracehttp` | JS `@opentelemetry/sdk-trace-base` + `exporter-trace-otlp-proto` | Python `opentelemetry-sdk` + `exporter-otlp-proto-http` |
|---|---|---|---|---|
| Root test span (I-§6.1) | `start_span(..., with_parent: Context.empty)` | `tracer.Start(ctx, name, trace.WithNewRoot())` | `tracer.startSpan(name, { root: true })` or pass `ROOT_CONTEXT` | `tracer.start_span(name, context=Context())` (an empty context, **not** `None`, which means "current") |
| Private provider (I5) | `TracerProvider.new(sampler:, id_generator:, resource:, span_limits:)` | `sdktrace.NewTracerProvider(WithSampler(AlwaysSample()), WithIDGenerator(g), WithResource(r), WithSpanLimits(l), WithSpanProcessor(bsp))` — **not** `otel.SetTracerProvider` | `new BasicTracerProvider({ sampler: new AlwaysOnSampler(), idGenerator, resource, spanLimits, spanProcessors: [bsp] })` — **not** `api.trace.setGlobalTracerProvider` | `TracerProvider(sampler=ALWAYS_ON, id_generator=g, resource=r, span_limits=l)` then `add_span_processor(bsp)` — **not** `trace.set_tracer_provider` |
| CSPRNG IDs (I5) | `SecureRandomIdGenerator` | `WithIDGenerator(custom)`; default uses `math/rand/v2` | `idGenerator` option; default `RandomIdGenerator` uses `Math.random` | `id_generator=` option; default `RandomIdGenerator` uses `random.getrandbits` |
| Span limits (§9.1) | `SpanLimits.new(attribute_count_limit: 128, attribute_length_limit: 10243, event_count_limit: 100, event_attribute_count_limit: 128, event_attribute_length_limit: 16384)` | `WithSpanLimits(SpanLimits{AttributeCountLimit:128, AttributeValueLengthLimit:10243, EventCountLimit:100, AttributePerEventCountLimit:128})` — non-positive means "SDK default", use `WithRawSpanLimits` to express zero. **Go applies no value-length limit to event attributes at all** (`addEvent` applies only the per-event count and depth limits) — so the collector must truncate `exception.message` (10,243) and `exception.stacktrace` (16,384) itself before adding the event | `spanLimits: { attributeCountLimit: 128, attributeValueLengthLimit: 10243, eventCountLimit: 100, attributePerEventCountLimit: 128 }` — does not read env when set explicitly; no separate event value-length knob — truncate collector-created event strings yourself, as for Go | `SpanLimits(max_attributes=128, max_span_attribute_length=10243, max_events=100, max_event_attributes=128, max_attribute_length=16384)` — explicit non-None wins over env; `max_attribute_length` is the event/link value cap, `max_span_attribute_length` the span one |
| Attribute overflow | keeps earliest | drops new key | drops new key | **evicts oldest** (§14.1) |
| Batch processor (§9.1) | `BatchSpanProcessor.new(exp, max_queue_size:, max_export_batch_size:, schedule_delay: 1000, exporter_timeout: 30000, metrics_reporter:)` | `NewBatchSpanProcessor(exp, WithMaxQueueSize, WithMaxExportBatchSize, WithBatchTimeout(1*time.Second), WithExportTimeout(30*time.Second))`; queue-full drops **new** span | `new BatchSpanProcessor(exp, { maxQueueSize, maxExportBatchSize, scheduledDelayMillis: 1000, exportTimeoutMillis: 30000 })`; queue-full drops **new** span | `BatchSpanProcessor(exp, max_queue_size=, max_export_batch_size=, schedule_delay_millis=1000, export_timeout_millis=30000)`; queue-full evicts **oldest** |
| Exporter, explicit everything (I6) | `OTLP::Exporter.new(endpoint:, headers:, compression: "gzip", ssl_verify_mode:, certificate_file: nil, …)` | `otlptracehttp.New(ctx, WithEndpointURL(u), WithHeaders(h), WithCompression(GzipCompression), WithEncoding(EncodingProtobuf), WithTimeout(30*time.Second), WithRetry(cfg))` + `WithTLSClientConfig(…)` **only when `u` is https** (`Start` fails with `insecure HTTP endpoint cannot use TLS client configuration` for the http relay). User options are applied after env, so each explicit option wins; anything you leave out is read from env — in particular `OTEL_EXPORTER_OTLP_{,TRACES_}PROTOCOL=http/json` would switch the body to JSON (receiver → 415), hence `WithEncoding`. **Parse and validate `u` first**: `WithEndpointURL` with an unparsable URL logs and keeps the prior (env-derived!) endpoint. Headers **replace** env headers | `new OTLPTraceExporter({ url, headers, compression: CompressionAlgorithm.GZIP, timeoutMillis })` — `url` and `compression` win; **`headers` are merged with `OTEL_EXPORTER_OTLP_(TRACES_)HEADERS`** (explicit wins per key, env-only keys still sent); CA / client-cert env (`OTEL_EXPORTER_OTLP_{,TRACES_}{CERTIFICATE,CLIENT_KEY,CLIENT_CERTIFICATE}`) is still read for https | `OTLPSpanExporter(endpoint=, headers=, compression=Compression.Gzip, timeout=)` — on `main` **`headers` merged with env** (explicit wins per key, env-only keys still sent); on ≤ 1.36 explicit headers *replaced* env. `certificate_file=None` / omitted client-cert options **fall back to the env vars**, they do not disable them |
| Defeating env leakage (headers, certs, protocol) | n/a | explicit options cover it (see row above) | [P] **Do not** poke private fields (`_delegate`, transport `_parameters` — unsupported, moved between versions). Construct the exporter inside a scope that removes **every** `OTEL_EXPORTER_OTLP_*` key from `process.env` and restores them in `finally` (Node is single-threaded, so this is safe). Test (§16): set `OTEL_EXPORTER_OTLP_HEADERS=authorization=garbage,x-env-only=1` and `OTEL_EXPORTER_OTLP_TRACES_HEADERS=authorization=garbage2,x-env-traces=1` and assert the request carries exactly `Authorization` (ours), `Buildkite-Tests-Run-Key`, `Buildkite-Tests-Span-Stream` and no `x-env-*`; compare names case-insensitively. | [P] Same scrub of `os.environ` around construction (private header map moved from `_session.headers` to `_client._headers` — not a contract). Caveat: `os.environ` is process-global; a customer thread constructing its own exporter at the same instant would see the scrubbed env — acceptable at suite start, document it. Python reads `TRACES_HEADERS` **or else** general headers (one or the other, not merged with each other). Same test. |
| Retry (§4) | exporter built-in, 6 attempts | `WithRetry(RetryConfig{Enabled, InitialInterval 5s, MaxInterval 30s, MaxElapsedTime 1m})`; retries 429/502/503/504 (+ `Retry-After`), not 408 | built-in, 5 retries, 429/502/503/504 (+ `Retry-After`), not 408 | `main`: built-in, 6 attempts, 429/502/503/504 (+ `Retry-After`), not 408. ≤ 1.36: retried 408 + **every 5xx**, no `Retry-After` — pin newer |
| Processor hooks for the forwarder (§8.2) | `on_start(span, parent_context)`, `on_finish(span)` | `OnStart(parent context.Context, s ReadWriteSpan)`, `OnEnd(s ReadOnlySpan)`; parent span via `trace.SpanContextFromContext(parent)` | `onStart(span, parentContext)`, `onEnd(span)`; parent via `trace.getSpanContext(parentContext)` | `on_start(span, parent_context)`, `on_end(span)`; parent via `trace.get_current_span(parent_context)` |
| Context key for the test span (§8.1) | `OpenTelemetry::Context.create_key` + `Context.with_values` | `context.WithValue(ctx, privateKeyType{}, …)` — key type unexported | `createContextKey('buildkite.test.execution')` + `context.with(ctx.setValue(k, v), fn)` | `opentelemetry.context.create_key` + `attach(set_value(k, v))` / `detach(token)` |
| Global provider detection (§8.4) | `OpenTelemetry.tracer_provider.is_a?(Internal::ProxyTracerProvider)` | Before anyone calls `otel.SetTracerProvider`, `otel.GetTracerProvider()` returns the unexported delegating `*global.tracerProvider`; **after** `SetTracerProvider(tp)` it returns `tp` itself (`globalTracer.Store(tracerProviderHolder{tp})`), so `tp.(*sdktrace.TracerProvider)` succeeds for a suite-owned SDK provider → `RegisterSpanProcessor(forwarder)`. An unrecognised type is ambiguous: "nobody configured OTel" (the wrapper) vs a custom provider — and only the wrapper is replaceable. ⚠️ Decision for §14.5: offer `WithTracerProvider(tp)` explicitly; when absent, attach if the global type-asserts to `*sdktrace.TracerProvider` or to `interface{ RegisterSpanProcessor(sdktrace.SpanProcessor) }`, otherwise treat it as unconfigured **only if** `reflect.TypeOf(tp).PkgPath() == "go.opentelemetry.io/otel/internal/global"`, else warn and keep test-only export. **Never replace an unknown provider.** | `api.trace.getTracerProvider()` is a `ProxyTracerProvider`; `.getDelegate()` returns the `NoopTracerProvider` singleton when nobody registered one (`ProxyTracerProvider` is deprecated for the next API major — re-check when pinning). **Two JS-specific gaps:** (a) sdk-trace 2.x providers take processors only in the constructor — there is no public `addSpanProcessor`, so §8.4's runtime attach to a suite-owned provider is impossible; the collector must export its forwarder (e.g. `createBuildkiteSpanProcessor()`) for the customer to put in `spanProcessors`, and when the delegate is a suite-owned provider without it, warn and keep test-only export [P]. Never mutate private processor arrays. (b) A bare `BasicTracerProvider` installs **no context manager**; with `NoopContextManager`, `context.with()` runs the callback but ignores the supplied context, so `current_test_span`, `annotate` and child association silently stop working. Collector-managed setup must register `AsyncLocalStorageContextManager` (`NodeTracerProvider.register()` does; or `context.setGlobalContextManager`) — never over a customer-installed one. Test across an `await`. | `trace.get_tracer_provider()` returns `ProxyTracerProvider` until set; `_TRACER_PROVIDER_SET_ONCE` makes a second `set_tracer_provider` a warning no-op (`Overriding of current TracerProvider is not allowed`) — so detect **before** installing, and never call `set_tracer_provider` when suite-owned |
| Flush / shutdown with deadline (§10.2) | `force_flush(timeout:)`, `shutdown(timeout:)` | `ForceFlush(ctx)`, `Shutdown(ctx)` honour the ctx deadline — pass one `context.WithDeadline` for the shared 30 s budget | `forceFlush()`, `shutdown()` take **no** timeout; each batch gets `exportTimeoutMillis`, no total — race the Promise against your own 30 s timer shared by both streams; losing the race stops waiting, not exporting (same "pending" caveat as Python) | `BatchSpanProcessor.force_flush(timeout_millis)` **ignores** the timeout (SDK TODO), drains every batch and ignores exporter results; `shutdown()` has an internal 30 s join per processor (two processors = two joins), then stops draining. Run flush/shutdown on a daemon thread and `join` it against **one** monotonic deadline shared by both streams; when the deadline passes, stop *waiting* — the export thread keeps running (bounded waiting is not cancellation), so mark the remainder "pending", not "dropped" |
| Drop/export metrics (§9.4) | SDK `metrics_reporter` hooks | No callback. Current `main` has **opt-in** self-observability metrics (`otel.sdk.processor.span.processed{error.type=queue_full}`), absent in older releases and needing a meter provider | Same: private `_droppedSpansCount`, `diag.warn`, opt-in self-obs meter on current `main` only | Same: `logger.warning("Queue full, dropping …")`, opt-in via `OTEL_PYTHON_SDK_INTERNAL_METRICS_ENABLED` on current `main` only |

The last row means drop accounting (§9.4, I9) **cannot** be lifted from the SDK in Go/JS/Python; the collector computes it, and two Ruby behaviours cannot be reproduced exactly [P — departures for Matt]:

- Counters: wrap the processor's `on_end` to count `offered`; wrap the exporter (`SpanExporter` is public in all three) to count `handed`, `succeeded`, `failed` per span, and remember the last failure cause. Cause: Go `Export` errors and JS `ExportResult.error` carry the HTTP status; Python `export()` returns only `SUCCESS`/`FAILURE`, so capture the status from the exporter's logger (`Failed to export span batch code: …`) via a `logging` handler, or report `unknown`.
- Loss is only known once the drain has **completed**: after `shutdown` returns (not timed out), `offered − handed` = never reached the exporter (queue overflow or shutdown cut-off — the private queue does not distinguish `buffer-full` from `terminating`; use one reason, e.g. `not-exported`), `failed` = `export-failure`. If the deadline fired first, report `offered − handed − still-possibly-exporting` as **pending/unknown**, not as dropped, and keep the counters alive until the export thread settles rather than resetting them for the next suite run.
- Ruby's **immediate** first-drop warning needs a queue-full hook the SDKs do not expose (their self-observability metrics are opt-in and version-specific). Departure: warn at suite-end flush and at exit only, with the same `TEST RESULTS MISSING: OpenTelemetry dropped N test.execution span(s) (<reason>…)` wording. Test it with queue size 2, 5 spans ended inside one batch delay, then flush.

Portable rule that falls out of the span-limits row: the collector truncates its **own** event strings (`exception.message` to 10,243 chars, `exception.stacktrace` to 16,384) before `add_event`, in every target, and treats SDK limits as a second line of defence.

### 14.1 Common to all targets

- **CSPRNG IDs everywhere.** Test suites seed PRNGs (pytest-randomly seeds `random`; Jest/JS code can stub `Math.random`; Go tests may seed `math/rand`). Use `crypto/rand`, `secrets`/`os.urandom`, `crypto.randomBytes`. Reject all-zero IDs.
- **Realtime end timestamp.** Capture the end with the OS realtime clock immediately after the test returns, before the reporter runs. Suites that freeze time (`freezegun`, `jest.useFakeTimers`, `Timecop`) must not corrupt it. Fall back to the SDK clock if end < start.
- **Result mapping is explicit.** Write the full table of framework outcomes → `pass|fail|skipped` in the implementation and in tests. Anything unmapped must not produce a span with an unknown status (I2).
- **Scope/name parity** (§13) is a blocking first step for the three targets that already have a JSON path (Jest, Playwright, pytest). Go has none (§14.5): its identity must be *decided* first, not matched.
- **Who owns the JSON fallback in worker models** (Jest, Playwright, xdist) — ⚠️ not decided. In Ruby the JSON reporter and the OTel setup live in the same process, so §1.1's per-process decision is self-contained. Where tests run in workers but the JSON upload is done by a main-process reporter, a worker that falls back has no JSON path of its own. Options the implementer must choose between and record: (a) workers signal fallback to the main reporter (e.g. via the test result payload) and the main reporter uploads JSON only for tests from fallen-back workers; (b) mode is decided once in the main process before workers start (run key valid, SDK importable, exporter constructible) and passed to workers as config; a worker whose own setup then fails treats it like a post-start failure — warn `TEST RESULTS MISSING` for that worker's tests, no fallback; (c) workers do their own JSON upload in fallback. Prefer (b): one decision per run, matches the spirit of §1.1, and keeps "never both channels in one run" true at the cost of losing results from a worker that fails late (which §11 already makes visible).
- **Attribute order** only matters where the SDK keeps the *earliest* attributes at the count limit. [V upstream source, Oct 17 2026] Go and JS drop the **new** key once the count limit is hit (same as Ruby → keep §6.2's start-order); Python's `BoundedAttributes` **evicts the oldest** key instead, so in Python the §6.2 ordering would evict `via`/`run_key`/status first if customer code adds >125 attributes to the test span. The Python port must therefore make the synthesis attributes the **newest**: at finish, after `describe_test`, re-set `buildkite.execution.via`, `buildkite.run_key`, `test.case.result.status`, `buildkite.test.scope` and `buildkite.test.name` as the final writes (Ruby's `describe_test` re-sets scope/name/run_key but **not** via/status — add those), and test the 129-attribute case asserting the server-required keys survive. Pin the count limit (128) on the private provider in every target regardless.
- **Headers per stream**: two exporter instances with different `Buildkite-Tests-Span-Stream` values; never one exporter with mutable headers.
- **Never read `OTEL_EXPORTER_OTLP_*`**: in Go, JS and Python SDKs the OTLP exporter reads env *by default*, and JS/Python **merge** env headers into explicit ones. Construct exporters with explicit endpoint/headers/compression/TLS options, defeat the header merge as §14.0 describes, and confirm (with a test that sets the env vars to garbage) that they are not consulted.
- **Collector name / test_runner strings**: `js-buildkite-test-collector` + `jest`/`playwright`; `python-buildkite-test-collector` + `pytest`; Go: choose `go-buildkite-test-collector` + framework name and **file a ta-ingestion change** to register the selector deriver (⚠️ §15).
- **Prefix for warnings**: use the target's existing stderr prefix.

### 14.2 Python pytest

- Hooks are in-process and per xdist worker (`pytest_runtest_logstart`, `pytest_runtest_makereport`, `pytest_collectreport` already used by the JSON plugin). Suite start ≈ `pytest_configure`/`pytest_sessionstart`; suite end ≈ `pytest_sessionfinish`; process exit ≈ `atexit` or `pytest_unconfigure`.
- Natural wrapper: a `pytest_runtest_protocol` hookwrapper (start span, set context var, yield, capture end) with phase spans from `pytest_runtest_setup/call/teardown` hookwrappers (setup→`test.setup`, call→`test.body`, teardown→`test.teardown`).
- Result mapping proposal: passed→`pass`; failed/error (any phase)→`fail`; skipped→`skipped`; xfail (expected failure)→`skipped`; xpass strict→`fail`, xpass non-strict→`pass`. Match whatever the JSON plugin already maps.
- xdist: each worker is its own process with its own run key copy; the controller must not also emit spans. Fork-per-test runners (`pytest-forked`) lose the `makereport` hook — the JSON plugin already works around this; mirror it.
- Context: use `contextvars` via the OTel Python context API; thread/async tests inherit it.
- HTTP-stub tooling to exempt or document: `responses`, `vcrpy`, `pytest-httpx`, `requests-mock`.

### 14.3 JS Jest

- Jest reporters (`onTestResult`, `onRunComplete`) run in the **main** process; tests run in worker processes. A test span started in the main reporter cannot be the active context for instrumentation in the worker, and the end timestamp would be post-hoc.
- Proposal: a `testEnvironment` or `setupFilesAfterEach`-style hook in the worker that wraps each test (`beforeEach`/`afterEach` or the environment's `handleTestEvent` for `test_start`/`test_done`), owns the private provider, and exports from the worker. Each worker holds its own exporters and headers; the main-process reporter does no OTel work.
- Flush: workers must `forceFlush` at `test_done` of the last test / environment teardown; Jest kills workers abruptly. Budget a shutdown in `teardown()` of the environment.
- Result mapping: passed→`pass`, failed→`fail`, skipped/pending/todo/disabled→`skipped`. Retries (`jest.retryTimes`) produce one span per attempt.
- Scope/name: Jest JSON uses `ancestorTitles.join(' ')` and `title`; reuse the collector's existing functions.
- Fake timers: `jest.useFakeTimers()` overrides `Date.now`; capture timestamps with `process.hrtime.bigint()` + a realtime offset taken before the test, or `performance.timeOrigin`-based calculation that fake timers do not touch. Verify.
- Stub tooling: `nock`, `msw` — either allow the OTLP endpoint or use an HTTP client path that bypasses them (undici `fetch` is not intercepted by `nock` < 14).
- SDK 2.x gaps (§14.0): no runtime `addSpanProcessor` → suite-owned child export needs the customer to add the collector's exported span processor at provider construction, else warn + test-only export; collector-managed setup must register an async context manager (`NodeTracerProvider.register()`), otherwise `context.with` is a no-op and child association breaks. Both apply to Playwright too.

### 14.4 JS Playwright

- Same main-vs-worker split: the reporter (`onTestEnd`, `onEnd`) runs in the main process; tests run in worker processes. Proposal: a Playwright **fixture** (auto-use, worker-scoped setup + test-scoped wrapper) owns the provider and emits spans from the worker; the reporter stays JSON-only or is skipped in OTel mode.
- Phases: `test.setup` ≈ `beforeEach` hooks + fixture setup; `test.body` ≈ the test function; `test.teardown` ≈ `afterEach` + fixture teardown. Playwright exposes `testInfo` with step timings; decide how far to go.
- Retries: one span per attempt; the JSON Playwright reporter reuses identity across retries — keep scope/name the same across attempts but give each attempt its own external id.
- Result mapping: passed→`pass`; failed/timedOut/interrupted→`fail`; skipped→`skipped`; `expectedStatus` ≠ status (flaky pass after retry) still maps per attempt.
- Browser-side spans (page JS) are out of scope; only Node-side instrumentation is captured.

### 14.5 Go

- There is no Go collector and no reporter API in `go test`. Options: (a) a library the customer calls from `TestMain` + a per-test helper (`bktest.Run(t, func(t *testing.T))` or `t.Cleanup`-based), (b) a `go test -json` post-processor (no active context → no child spans, phases impossible). Prefer (a) for parity with the Ruby feature set; (b) cannot satisfy §7/§8.
- Parallel tests (`t.Parallel()`) and subtests need per-goroutine context: pass `context.Context` into the test via the helper; spans from instrumentation must use that ctx.
- Scope/name: `t.Name()` gives `TestX/subtest`; propose scope = package path, name = `t.Name()`. There is no JSON identity to match, so define it once and register the primary selector deriver in ta-ingestion.
- **Prerequisite decisions ⚠️ (make and record before coding; the §17 template's JSON-related lines do not apply to Go):** (1) identity — scope/name/location as above, written down as the canonical definition; (2) what `OTelEnabled=false` means when there is no JSON path — propose: the library is a no-op (no upload at all), since Go has no legacy channel to fall back to; (3) what a mandatory-setup failure means — propose: warn with the §11 "disabled" wording and run as a no-op, never block `go test`; (4) global-provider ownership — Go can recognise a suite-owned `*sdktrace.TracerProvider` but cannot distinguish "nobody configured OTel" from a custom provider without reflecting on an unexported type (§14.0): propose `WithTracerProvider(tp)` = suite-owned (attach the forwarder, leave sampler/resource alone); absent → attach to a recognised SDK provider, install our own only when the global is provably the SDK's default delegating wrapper, otherwise warn and keep test-only export — never replace an unknown provider; (5) the Go toolchain floor the OTel dependencies impose on the module. §13 (identity parity) and the "keep the JSON path working" requirement are **not applicable** to Go.
- Result: `t.Failed()`→`fail`, `t.Skipped()`→`skipped`, else `pass`. Panics recovered by the helper → `fail` with the panic as the exception event.
- Shutdown: `TestMain` is the only reliable place to flush/shutdown; `go test` per-package processes are short-lived, so budget the 30 s flush there.
- SDK: `go.opentelemetry.io/otel/sdk/trace` + `otlptracehttp` with `WithEndpointURL`, `WithHeaders`, `WithCompression(GzipCompression)`, and **no** env fallback (the Go exporter reads `OTEL_EXPORTER_OTLP_*` unless options are given; set every option explicitly and test it).

---

## 15. Known gaps and open items ⚠️

| Item | Status | Owner / where |
|---|---|---|
| Receiver does not act on `Buildkite-Tests-Span-Stream` yet | TE-7322 | bk/bk `Analytics::API::TracesController` |
| bktec relay (`internal/otlprelay/relay.go`) did not forward `Buildkite-Tests-Span-Stream` upstream — child spans sent through the relay arrived unlabelled (= `test` under the intended absent-means-test rule) | fix in draft PR [buildkite/test-engine-client#698](https://github.com/buildkite/test-engine-client/pull/698) (branch `otlp-relay-forward-span-stream`): header forwarded verbatim incl. retries, absent/empty not sent, no validation. Not merged, not released | test-engine-client |
| `buildkite.test.execution.external_id` is never read by the synthesizer (`testResult.externalId` unset); OTel executions get server-generated ids | not ticketed | ta-ingestion |
| No primary-selector registry entry for a Go collector | needs a ta-ingestion PR when Go collector name is chosen | ta-ingestion `JsonPrimarySelector.DERIVERS` |
| `partialSuccess` is neither sent nor read | fine for now | — |
| Ruby collector 2.17.0 was released Oct 16 (D113); the four targets (JS Jest/Playwright, Python pytest, Go) have no OTel code [V] — `@opentelemetry/api` in the JS lockfile is a transitive vitest dependency. The Swift collector is OTLP-only by design and is **not** a port of this spec | this doc | — |
| Customer docs for non-Ruby collectors | none | docs repo, pattern in `rspec-otel-setup.md` |

---

## 16. Acceptance checklist (port each row as a test)

Derived from the Ruby spec titles (`spec/test_collector/otel_spec.rb`, `otel/*_spec.rb`, `rspec_plugin/otel_enabled_spec.rb`, `rspec_plugin/phase_spans_spec.rb`, `rspec_plugin/trace_spec.rb`, `test_collector_spec.rb`). Group names are the doc sections they verify. Every row should fail against a plausible wrong implementation; do not write tests that only assert "no crash".

**Configuration (§3)**
- leaves OTel off unless opted into
- stores OTel options without setting up the SDK during configure
- rejects a span filter that cannot be called
- leaves OTel off without a credential, like the JSON path (no warning)
- prefers the relay token (`BUILDKITE_TESTS_OTLP_TOKEN`) over the JSON token; treats a blank relay token as unset
- takes the relay endpoint and credential from `BUILDKITE_*` variables; can override the endpoint for local development
- warns and falls back to JSON for an unsupported framework
- warns and falls back to JSON when the run key is invalid, before loading dependencies or registering providers/shutdown hooks; accepts boundary lengths (1 and 255) and printable ASCII punctuation
- says results will not be uploaded when only the relay token held the credential
- falls back to JSON when OTel is enabled but could not be configured (e.g. SDK missing)

**Wire (§4, §9)**
- sends the run key, stream, and token as request headers; labels every request **including retries** with the stream of its spans
- ignores standard OTLP header/endpoint/compression/certificate/TLS env variables even for its own endpoint; pins gzip
- uses an always-on sampler, CSPRNG IDs, and the producer resource for test spans
- uses collector batch/queue settings independently of `OTEL_BSP_*`; accepts overrides up to 2³¹−1; warns once per setup evaluation and uses the default for invalid values; warns once per setup evaluation and resets both when batch > queue (and warns again on a reconfigure after shutdown); child overrides do not affect test sizes and vice versa
- truncates span attributes in characters independently of `OTEL_SPAN_*`; preserves identity and result despite `OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT`; truncates exception attributes and retains the newest 100 events
- names each non-fatal export exception class once, fails the batch, and records its cause; re-raises fatal exceptions from export and lifecycle boundaries
- recovers both batch workers after a stubbing-policy reset and tolerates blocked exports at exit

**Test span (§6)**
- starts the test span as a trace root and links it to the Agent job trace; skips missing or malformed trace context
- reserves synthesis attributes (via, run_key, status placeholder) before descriptive fields, run metadata, and tags
- marks every test span as the submission, even one whose test describes nothing
- records what the test was and how it went; records failure as span status and exception events; caps failure reason at 1,024 and exception message at 10,243 while leaving stack traces to the event limit
- describes the test using the same scope/name/path as the JSON upload (with and without location prefix; shared/parameterised cases use the call site)
- marks a pending/skipped example as `skipped`; classifies an unexpectedly passing pending example as failed (framework-specific)
- falls back to the SDK clock when the captured end precedes the span start
- finishes the span even when the test cannot be described; fails open and reports a missing result when starting a span fails
- adds configure-level tags and per-test tags as `buildkite.tag.*`; per-test wins
- keeps producer identity on the resource and run metadata on the test span; uses provider-native CI run IDs rather than the run key; keeps configured URLs on the execution only when they differ from the CI resource URL
- tags executions with `ci.worker.id` from the agent id; omits when unset/blank; explicit tag overrides it
- annotations become `test.annotation` events on the test span, not the current span; no-op outside a test or when off
- exports a failure from a hook that never runs the test body (before-all / outer wrapper) without duplicating normal results
- uploads **no JSON** when OTel is enabled; uses only the JSON path when OTel is disabled

**Phases (§7)**
- adds setup, body, teardown as direct children of the test span, in order; nests instrumentation under the phase it ran in; around-hook spans sit under the test span
- fails the setup span and skips the body span when a before hook raises; does not fail setup when a hook skips the test
- fails only the body span when the test raises; only teardown when an after hook raises; attributes each failure to its own phase; when both the body and an after hook fail, body records the body failure and teardown records only the after-hook failure
- starts fresh phases for each run of the same test; leaves the context as it found it
- exports nothing for a passing test with no instrumentation; exports a phase iff it grouped a tracked child **or** its status ≠ UNSET — a populated phase is exported past a filter that drops everything; a failed empty phase is exported; an empty phase with status UNSET is not
- adds no phase spans when OTel is disabled

**Child spans (§8)**
- forwards only spans from the execution trace; remembers an accepted span until it finishes
- forwards only spans accepted by the filter; runs the filter without holding the lock; retains spans the filter itself finishes; drops a span whose filter is still running at shutdown
- forwards a phase span with children without consulting the filter; drops a phase that grouped nothing and has status UNSET; forwards a failed empty phase; forgets a phase's children once it finishes; does not retain a finished phase when a child starts late
- retains spans when the filter fails, warning once; filters child spans without filtering test spans
- becomes inert on shutdown without shutting down the child processor; does not block span completion during a flush; does not expose child processor failures to the suite; keeps the test span when the child queue overflows
- configures collector-managed children without suite OTel (installs instrumentation, CSPRNG ids); leaves the SDK and instrumentation alone when the suite owns its provider; keeps test-span export enabled with an incompatible suite provider; exports test spans when the suite's sampler drops children; keeps private test-span export alive if the suite shuts down its provider
- sets up after application libraries have loaded / after the application configures its provider

**Lifecycle (§10)**
- flushes test spans and children against one shared deadline; still flushes children when the test flush fails
- deactivates children, then shuts down test spans and children against one deadline; attempts child shutdown when test shutdown fails
- registers a process-lifetime exit shutdown once, on successful configure; shuts down the execution processor when private provider setup fails; keeps test-span export when child setup fails; makes a partially attached forwarder inert
- token refresh: refreshes `Authorization` when reconfigured with a new token; warns when reconfigured with a different run key, keeping the original; keeps the current token when reconfigured without one
- configure A → shutdown → configure B (different run key): no mismatch warning, B's key is used, no duplicate exit handler
- warns instead of propagating a non-fatal exception from flush/shutdown

**Drop accounting (§9.4, §11)**
- warns only once when test spans are dropped; names the HTTP status or error that caused an export failure; forgets a failure once a retried export succeeds; does not blame an earlier failure for a full buffer; counts spans that failed to start together with dropped spans; ignores other metrics
- reports every test span the run dropped at flush/exit; stays silent when the first warning already covered them; starts a fresh count per suite run; stays silent when nothing was dropped
- warns about dropped child spans as best-effort, not as missing results

**End-to-end**
- one real run against a local receiver (or recorded request) showing: two request kinds (test/child headers), a passing span, a failing span with status + exception event, a skipped span, phases exported only when they grouped a tracked child or have non-UNSET status, and no `/v1/uploads` call.
- Harness: a loopback HTTP server that gunzips the body, decodes `opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest` (the proto packages ship with every SDK's exporter), records headers per request, and replies `200` with an empty `ExportTraceServiceResponse`. Point `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` at it and set `BUILDKITE_TESTS_OTLP_TOKEN`. Assert the **expected count** of `test.execution` spans (one per test the suite ran — a harness that only inspects whatever arrived passes when results are missing) and, for every one of them, each synthesis prerequisite from §12: `parent_span_id` empty; `buildkite.execution.via = "otlp"`; `buildkite.test.name` non-empty; `buildkite.run_key` equal to the request's `Buildkite-Tests-Run-Key`; `test.case.result.status` ∈ {pass, fail, skipped}; `0 < start_time_unix_nano ≤ end_time_unix_nano`; `code.line.number` is an int value. Stream header matches the span kind in every request including a forced retry (reply `503` once, compare header names case-insensitively). Run once more with `OTEL_EXPORTER_OTLP_HEADERS=authorization=garbage,x-env-only=1`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS=authorization=garbage2,x-env-traces=1`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/json`, `OTEL_TRACES_SAMPLER=always_off`, `OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT=2` all set: same spans arrive, protobuf body, no `x-env-*` header, our `Authorization`. Run once with queue size 2 to exercise the overflow warning. A 200 from the loopback server proves the wire shape, not synthesis; the real-server check needs an org with `Feature::TestEngineOTLPTraceIngest` (else every request is `403`) — ask Matt for a flagged test org before the first live run and confirm the executions appear in Test Engine.

---

## 17. Implementer prompt template

Paste into a new thread in this repo (`buildkite/bktest`, branch `otel-collector-spec` or wherever this file has been merged):

```
Implement OTel-only mode for the <Jest|Playwright|pytest|Go> collector in this repo, matching the
portable spec in docs/collector-otel-mode-spec.md. Read §0–§1, §12 and §13 first, then build in §10.3 order using
the SDK mechanisms in §14.0.

Requirements:
- Same public interface: `otel_enabled` (default off) and `otel_span_filter` options; env vars in §3.2
  with identical names; never read OTEL_EXPORTER_OTLP_* for Buildkite export.
- Same wire contract (§4): protobuf+gzip, Authorization "Token token=", Buildkite-Tests-Run-Key,
  Buildkite-Tests-Span-Stream test|child from two separate exporters.
- Same span model: §5 resource, §6 test.execution attributes and ordering, §6.5 result/failure
  mapping, §7 phase spans, §8 child-span forwarder + filter + BYO provider rules.
- Same lifecycle and fail-open behaviour (§10, §11): deferred setup; JSON fallback only when the
  mandatory test-span path cannot be configured (child-span path failure → warn and continue with
  test spans only); flush at suite end, shutdown once at exit, 30 s shared budget, TEST RESULTS
  MISSING vs best-effort child warnings.
- [Jest/Playwright/pytest only] Identity parity (§13) is the first deliverable: reuse the JSON
  path's scope/name/location functions and prove the (scope, name) pairs match in a real run.
  Keep the JSON path working and unchanged when otel_enabled is off.
- [Go only] There is no JSON path. Make and record the §14.5 prerequisite decisions (identity,
  meaning of disabled, meaning of setup failure, explicit global-provider ownership option) before
  writing code.
- [Worker-model targets: Jest, Playwright, pytest-xdist] Decide and record who owns the JSON
  fallback (§14.1); the spec prefers option (b).
- Start with §14.<n> as a hypothesis about this framework's execution model; verify it against
  the framework source before building, and write down what you found.
- Port §16 as the test plan, including the end-to-end harness. Each test must fail against a
  plausible wrong implementation. Prove I6 with OTEL_EXPORTER_OTLP_HEADERS set to garbage and I5
  with OTEL_TRACES_SAMPLER=always_off.
- Do not push, do not open a PR; commit on a branch and report: files changed, decisions you made
  on the spec's behalf, every [P] item you confirmed or refuted, and the end-to-end evidence from
  §16 "End-to-end".
```

---

## 18. Ruby constants cheat-sheet [V]

```
DEFAULT_ENDPOINT                      https://tests-otlp.buildkite.com/v1/traces
RUN_KEY_HEADER                        Buildkite-Tests-Run-Key
RUN_KEY_FORMAT                        /\A[!-~]{1,255}\z/
SPAN_STREAM_HEADER                    Buildkite-Tests-Span-Stream   (test | child)
EXECUTION_VIA_ATTRIBUTE               buildkite.execution.via       = otlp
RESULT_ATTRIBUTE                      test.case.result.status       pass | fail | skipped  (placeholder "unset")
TAG_ATTRIBUTE_PREFIX                  buildkite.tag.
TRACER_NAME                           buildkite-test-collector
TEST_SPAN_NAME                        test.execution
PHASE_SPAN_NAMES                      test.setup | test.body | test.teardown
PROCESSOR_TIMEOUT_SECONDS             30        (shared flush/shutdown budget)
EXPORT_TIMEOUT_MILLISECONDS           30_000
TEST_SPAN_MAX_QUEUE_SIZE              8_192
TEST_SPAN_MAX_EXPORT_BATCH_SIZE       120
TEST_SPAN_SCHEDULE_DELAY_MILLISECONDS 1_000
CHILD_SPAN_MAX_QUEUE_SIZE             2_048
CHILD_SPAN_MAX_EXPORT_BATCH_SIZE      512
CHILD_SPAN_SCHEDULE_DELAY_MILLISECONDS 5_000
SPAN_PROCESSOR_SIZE_LIMIT             2**31 - 1
TEST_SPAN_ATTRIBUTE_COUNT_LIMIT       128
TEST_SPAN_ATTRIBUTE_LENGTH_LIMIT      10_243   (chars)
TEST_SPAN_EVENT_ATTRIBUTE_LENGTH_LIMIT 16_384  (chars)
TEST_SPAN_EVENT_COUNT_LIMIT           100
FAILURE_REASON_MAX_LENGTH             1_024
EXCEPTION_MESSAGE_MAX_LENGTH          10_243
context keys                          buildkite.test.execution (trace id) / buildkite.test.execution.span (span)
annotation event                      test.annotation  attr buildkite.annotation
```
