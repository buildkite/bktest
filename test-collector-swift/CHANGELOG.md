# Changelog

## 2.0.0-beta.2

- Label execution root requests `Buildkite-Tests-Span-Stream: test` and child span requests `child`
- Disable export with a warning when the run key is not 1-255 printable ASCII characters without spaces, instead of sending requests the receiver rejects
- Log one reason when export is disabled: the generic `requires an API token` line no longer follows an invalid run key, endpoint, protocol or headers message
- Drop queued executions instead of retrying when the receiver returns a non-transient HTTP status (such as 401, 403, 413 or 500); only network errors, timeouts and 408/429/502/503/504 are retried. Drops are reported as `TEST RESULTS MISSING` with the cause, and the run total is reported at the bundle-end flush
- Truncate oversized failure reasons, exception messages, stack traces, annotations and attribute values (including run metadata and configured tags set when the execution starts) before export, so one huge value cannot get the request rejected
- Send a backlog of queued executions in batches of at most 120 spans and 512 KiB, retried per batch, so a rejected or oversized request drops only its own batch instead of every queued execution
- Give the bundle-end flush one 30 s budget shared by test executions (first) and child spans, instead of 30 s each, and warn when child spans are left unexported at the deadline
- Ignore `OTEL_EXPORTER_OTLP_*` while `BUILDKITE_ANALYTICS_TOKEN` or `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` is set, with a warning, so an application's OpenTelemetry configuration cannot redirect test results away from Test Engine; log the endpoint when the standard variables are used without Buildkite credentials
- Log endpoints, including in debug output, as scheme, host, port and path only, so credentials in an endpoint's user info, query or fragment never reach the logs
- Read `BUILDKITE_TESTS_OTLP_TOKEN` (blank = unset) ahead of `BUILDKITE_ANALYTICS_TOKEN`, so tests run by bktec still upload when bktec leaves the upload token blank; no `OTEL_EXPORTER_OTLP_*` warning when those variables point at the endpoint in use, as bktec's do
- Send the file name as `code.file.path`, as the 0.x JSON collector sent `file_name`, instead of the absolute path
- Document that `swift test --parallel` hides collector warnings, including `TEST RESULTS MISSING`, for workers without a failing test
- Make the bundle-end flush one immediate pass that never waits out the retry backoff, and bound every request, flush and child-span requests included, to the 10 s request timeout, so an unreachable or failing endpoint no longer holds every `swift test --parallel` worker process for about 20 s
- Name `BUILDKITE_TESTS_OTLP_TOKEN` in the ignored-`OTEL_EXPORTER_OTLP_*` warning, and document mapping it and `BUILDKITE_ANALYTICS_OTLP_ENDPOINT` into Xcode schemes for the bktec relay
- Document that only XCTest (including Quick and Nimble) is collected, that run-level tags are set through `BUILDKITE_ANALYTICS_TAGS` only, and how to keep a local `swift test --parallel` in one run

## 2.0.0-beta.1

- Replace proprietary execution uploads with XCTest execution root spans submitted through OpenTelemetry
- Export run and execution tags as `buildkite.tag.*` attributes
- Keep stable suite, CI worker/run, and VCS identity on OpenTelemetry resources while keeping Test Engine run metadata, framework details, custom metadata, and tags on each execution root
- Prioritize the three attributes required for execution synthesis when OpenTelemetry span attribute limits are constrained
- Support standard OTLP/HTTP exporter configuration and child-span forwarding
- Apply standard OTLP headers and protocols only with standard endpoints
- Warn when the unsupported `BUILDKITE_ANALYTICS_BASE_URL` setting is ignored
- Back off failed root-span exports instead of blocking every subsequent test
- Require Swift 5.10 and macOS 12 or newer

## 0.6.0

- Add tagging support at upload and execution levels
- Fix macOS CI: update test matrix to Swift 5.10, 6.1, 6.2

## 0.5.0

- Handle updated Upload API response with more permissive parsing and better errors
- Update documentation to use "Test Engine" instead of "Test Analytics"
- Add CONTRIBUTING.md
- Update CI to macOS 15 with newer Swift & Xcode versions

## 0.4.1

- Add `location` field to test executions
