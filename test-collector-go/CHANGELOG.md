# CHANGELOG

## Unreleased

* Initial release.
* `buildkite-test-collector` command: `go test -json ./... | buildkite-test-collector`
  reports every test to Buildkite Test Engine as OpenTelemetry `test.execution`
  spans over OTLP, prints the output as `go test -v` would, and exits with the
  tests' own status. Failing tests carry their output as the failure reason
  and detail; locations come from `go list` and the test files. `-quiet`
  skips the output replay for a `gotestsum --jsonfile` that was already
  printed; a JUnit file can be kept beside it either way round.
* `bktest` package: `bktest.Main` and `bktest.Start` report tests from inside
  the test process, with `test.body` phase spans and forwarded child spans on
  a separate stream, per-test `Tag` and `Annotate`, and a warning when a
  package is reported by both the package and the command.
* `TEST RESULTS MISSING` warnings when a result could not be delivered.
  Without a token the `bktest` package is silent; the command warns once.
