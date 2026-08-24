# Buildkite Test Engine Collectors

Official [Buildkite Test Engine](https://buildkite.com/platform/test-engine)
test collectors, coming together into one repository ✨

The collectors are moving in one at a time, each arriving with its full
git history. A collector whose directory exists here lives here — code,
issues, pull requests, and releases. Until then, its original repository
remains its home:

| Collector | Language | Home | Package |
|---|---|---|---|
| ruby | Ruby | [buildkite/test-collector-ruby](https://github.com/buildkite/test-collector-ruby) | [buildkite-test_collector](https://rubygems.org/gems/buildkite-test_collector) on RubyGems |
| python | Python | [buildkite/test-collector-python](https://github.com/buildkite/test-collector-python) | [buildkite-test-collector](https://pypi.org/project/buildkite-test-collector/) on PyPI |
| javascript | JavaScript | [buildkite/test-collector-javascript](https://github.com/buildkite/test-collector-javascript) | [buildkite-test-collector](https://www.npmjs.com/package/buildkite-test-collector) on npm |
| swift | Swift | [buildkite/test-collector-swift](https://github.com/buildkite/test-collector-swift) | BuildkiteTestCollector via SwiftPM |
| elixir | Elixir | [test_collector_elixir/](test_collector_elixir/) | [buildkite_test_collector](https://hex.pm/packages/buildkite_test_collector) on Hex |
| dotnet | .NET | [test-collector-dotnet/](test-collector-dotnet/) | [Buildkite.TestAnalytics](https://www.nuget.org/packages/Buildkite.TestAnalytics.Common) on NuGet |
| android | Android | [test-collector-android/](test-collector-android/) | com.buildkite.test-collector-android on Maven Central |
| rust | Rust | [test-collector-rust/](test-collector-rust/) | [buildkite-test-collector](https://crates.io/crates/buildkite-test-collector) on crates.io |

As each collector moves in, its Home link above changes to its directory
here, and the original repository is archived with a pointer back.

To get started, [create a test suite](https://buildkite.com/docs/test-engine)
and follow the README for your language's collector.

## About this repository

Each collector arrives with its full git history — once moved in,
`git log <its-directory>` traces back to that project's very first
commit — and its release tags namespaced by directory
(e.g. `test-collector-ruby/v2.9.0`).

CI runs per project: a change under a project's directory triggers that
project's own pipeline (`<project>/.buildkite/pipeline.yml`), uploaded by
the dispatcher in [.buildkite/pipeline.yml](.buildkite/pipeline.yml).

## Contributing

For a collector that lives here, issues and pull requests are welcome
right here! A PR touching one collector runs only that collector's CI,
and cross-collector changes are fine too — that's rather the point. For a
collector that hasn't moved in yet, use its original repository (its Home
link above). Each collector is licensed MIT; see the LICENSE file in its
directory.
