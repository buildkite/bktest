# Buildkite Test Engine Collectors

Official [Buildkite Test Engine](https://buildkite.com/platform/test-engine)
test collectors, together in one repository ✨

Every collector lives here — code, issues, pull requests, and releases —
each having arrived with its full git history:

| Collector | Language | Home | Package |
|---|---|---|---|
| ruby | Ruby | [test-collector-ruby/](test-collector-ruby/) | [buildkite-test_collector](https://rubygems.org/gems/buildkite-test_collector) on RubyGems |
| python | Python | [test-collector-python/](test-collector-python/) | [buildkite-test-collector](https://pypi.org/project/buildkite-test-collector/) on PyPI |
| javascript | JavaScript | [test-collector-javascript/](test-collector-javascript/) | [buildkite-test-collector](https://www.npmjs.com/package/buildkite-test-collector) on npm |
| swift | Swift | [test-collector-swift/](test-collector-swift/) | BuildkiteTestCollector via SwiftPM |
| elixir | Elixir | [test_collector_elixir/](test_collector_elixir/) | [buildkite_test_collector](https://hex.pm/packages/buildkite_test_collector) on Hex |
| dotnet | .NET | [test-collector-dotnet/](test-collector-dotnet/) | [Buildkite.TestAnalytics](https://www.nuget.org/packages/Buildkite.TestAnalytics.Common) on NuGet |
| android | Android | [test-collector-android/](test-collector-android/) | com.buildkite.test-collector-android on Maven Central |
| rust | Rust | [test-collector-rust/](test-collector-rust/) | [buildkite-test-collector](https://crates.io/crates/buildkite-test-collector) on crates.io |

The original single-collector repositories are archived with pointer
READMEs back to their directories here.

To get started, [create a test suite](https://buildkite.com/docs/test-engine)
and follow the README for your language's collector.

## About this repository

Each collector arrived with its full git history —
`git log <its-directory>` traces back to that project's very first
commit — and its release tags namespaced by directory
(e.g. `test-collector-ruby/v2.9.0`).

CI runs per project: a change under a project's directory triggers that
project's own pipeline (`<project>/.buildkite/pipeline.yml`), uploaded by
the dispatcher in [.buildkite/pipeline.yml](.buildkite/pipeline.yml).

## Contributing

Issues and pull requests are welcome right here! A PR touching one
collector runs only that collector's CI, and cross-collector changes are
fine too — that's rather the point. Each collector is licensed MIT; see
the LICENSE file in its directory.
