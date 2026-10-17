# Changelog

## 2.0.0-beta.2

- Send `Buildkite-Tests-Span-Stream: test` on execution root requests and `child` on child span requests so Test Engine can tell the two streams apart
- Disable export with a warning when the run key is not 1-255 printable ASCII characters without spaces, instead of sending requests the receiver rejects
- Stop retrying root-span exports after an HTTP status the receiver will keep returning (such as 401, 403, 413 or 500); keep retrying only transient failures (network errors, timeouts, 408, 429, 502, 503, 504)
- Warn `TEST RESULTS MISSING` with the count and cause when execution roots are dropped, and report the run total at the bundle-end flush
- Cap the failure reason at 1,024 characters and each exception message at 10,240 characters before export
- Add `docs/otel-spec-conformance.md`, the clause-by-clause audit against the portable OTel collector specification

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
