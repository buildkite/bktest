# CHANGELOG

## Unreleased

* Initial release: `bktest.Main` and `bktest.Start` report `go test` results
  to Buildkite Test Engine as OpenTelemetry `test.execution` spans over OTLP,
  with `test.body` phase spans and forwarded child spans on a separate
  stream, per-test tags and annotations, and `TEST RESULTS MISSING` warnings
  when a result could not be delivered.
