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

## Supported runners and features

Choose your runner below for setup instructions. JSON uploads are the default
unless noted. OpenTelemetry (OTel) submission sends test results as
`test.execution` spans over OTLP instead of JSON uploads; child spans show
the work performed within a test.

Custom tags are key-value metadata: run-wide tags apply to every test, while
per-test tags apply to individual executions; **Both** means both scopes are
supported. Annotations are custom messages recorded through a collector API,
not Buildkite pipeline annotations or arbitrary events added to your own OTel
spans.

**OTel child spans require a plan that includes them and are billed by data
ingested.** Test-result spans count as normal test executions.

| Collector / runner | JSON uploads | OTel submission | Child tracing (OTel) | Tags | Annotations |
|---|:---:|---|---|---|---|
| Ruby / [RSpec](test-collector-ruby/README.md#rspec) | Yes | [Experimental, opt-in](test-collector-ruby/docs/opentelemetry.md) | [Instrumented + manual](test-collector-ruby/docs/opentelemetry.md#choosing-instrumentation) | Both | JSON and OTel |
| Ruby / [Minitest](test-collector-ruby/README.md#minitest) | Yes | Not supported | Not supported | Both | JSON |
| Ruby / [Cucumber](test-collector-ruby/README.md#cucumber) | Yes | Not supported | Not supported | Both | JSON |
| Python / [pytest](test-collector-python/README.md) | Yes | [Experimental, opt-in](test-collector-python/docs/opentelemetry.md) | [Instrumented + manual](test-collector-python/docs/opentelemetry.md#child-spans-and-instrumentation) | Per-test | JSON and OTel (`spans` fixture) |
| JavaScript / [Jest](test-collector-javascript/README.md#jest) | Yes | [Experimental, opt-in](test-collector-javascript/docs/opentelemetry.md#jest) | [Instrumented + manual](test-collector-javascript/docs/opentelemetry.md#what-gets-recorded) | Run-wide | No collector API |
| JavaScript / [Vitest](test-collector-javascript/README.md#vitest) | Yes | [Experimental, opt-in](test-collector-javascript/docs/opentelemetry.md#vitest) | [Instrumented + manual (native tracing)](test-collector-javascript/docs/opentelemetry.md#vitest) | Run-wide | No collector API |
| JavaScript / [Playwright](test-collector-javascript/README.md#playwright) | Yes | [Experimental, opt-in](test-collector-javascript/docs/opentelemetry.md#playwright) | [Playwright steps](test-collector-javascript/docs/opentelemetry.md#playwright) | Both | No collector API |
| JavaScript / [Jasmine](test-collector-javascript/README.md#jasmine) | Yes | Not supported | Not supported | Run-wide | No collector API |
| JavaScript / [Mocha](test-collector-javascript/README.md#mocha) | Yes | Not supported | Not supported | Run-wide | No collector API |
| JavaScript / [Cypress](test-collector-javascript/README.md#cypress) | Yes | Not supported | Not supported | Run-wide | No collector API |
| Swift / [XCTest (including Quick/Nimble)](test-collector-swift/README.md) | No in 2.0 beta | [Default, OTLP-only (2.0 beta)](test-collector-swift/README.md#opentelemetry-behavior-and-delivery) | [Sampled instrumented + manual](test-collector-swift/README.md#opentelemetry-behavior-and-delivery) | [Both](test-collector-swift/README.md#execution-level-tags) | Deprecated API (OTel) |
| Elixir / [ExUnit](test_collector_elixir/README.md) | Yes | Not supported | Not supported | Not supported | JSON (`Tracing.measure`) |
| .NET / [xUnit](test-collector-dotnet/README.md) | Yes | Not supported | Not supported | Not supported | No collector API |
| Android / [Gradle unit tests](test-collector-android/README.md#step-3---unit-test-collector) | Yes | Not supported | Not supported | [Run-wide](test-collector-android/README.md#upload-level-tags) | No collector API |
| Android / [Instrumented tests](test-collector-android/README.md#step-4---instrumented-test-collector) | Yes | Not supported | Not supported | [Both](test-collector-android/README.md#execution-level-tags-instrumented-tests) | No collector API |
| Rust / [libtest / cargo test](test-collector-rust/README.md) | Yes | Not supported | Not supported | Not supported | No collector API |

For test splitting, automatic retries, muting and skipping, see
[bktec's runner support table](https://github.com/buildkite/test-engine-client#supported-runners-and-features).
Those are execution features, separate from collector support here.

When changing runner support or submission behavior, update this table and
the affected collector's guide together.

## About this repository

Each collector arrived with its full git history —
`git log <its-directory>` traces back to that project's very first
commit — and its release tags namespaced by directory
(e.g. `test-collector-ruby/v2.9.0`).

CI runs per project: a change under a project's directory triggers that
project's own pipeline (`<project>/.buildkite/pipeline.yml`), uploaded by
the dispatcher in [.buildkite/pipeline.yml](.buildkite/pipeline.yml).

## Releasing

See [RELEASING.md](RELEASING.md) for the current process and status of every
collector. Anyone can prepare a release pull request; publishing packages and
creating releases requires the appropriate maintainer permissions.

## Contributing

Issues and pull requests are welcome right here! A PR touching one
collector runs only that collector's CI, and cross-collector changes are
fine too — that's rather the point. Each collector is licensed MIT; see
the LICENSE file in its directory.

### Working in Amp orbs

[`.agents/setup`](.agents/setup) prepares Debian 12 Linux orbs with the
toolchains and dependencies for all eight collectors. Amp snapshots that
environment so fresh orbs can reuse it; a stale snapshot reruns the
idempotent setup using its existing caches. The initial download can take
several minutes. [`.agents/resume`](.agents/resume) intentionally does no
work: local tests need no authentication or backing services.

Orb-only versions are in [`.agents/mise.toml`](.agents/mise.toml), based on
the collectors' pins and CI versions. Setup installs prebuilt toolchains
and adds a checkout-scoped login-shell hook, so agents and supervised
services receive the same environment. This does not change local developer
defaults or replace the CI compatibility matrices. Keep the orb versions in
sync when changing a collector's toolchain requirements.

Run these commands from the corresponding collector directory:

| Collector | Local verification |
|---|---|
| JavaScript | `npm test -- --runInBand` |
| Python | `uv run --locked pytest` |
| Ruby | `bundle exec rake && bundle exec cucumber` |
| Rust | `cargo test --locked && cargo clippy --locked` |
| Elixir | `MIX_ENV=test mix test` |
| .NET | `dotnet test --no-restore Buildkite.TestAnalytics.Tests/Buildkite.TestAnalytics.Tests.fsproj` |
| Swift | `swift test --parallel --jobs 2` |
| Android | `support/scripts/lint && support/scripts/sdk-unit-tests` |

JavaScript includes Chromium and Cypress. Android includes JDK 17, SDK 34,
and SDK test/lint dependencies, but not an emulator or the example app's
published collector dependency. Setup accepts the Android SDK licenses;
running instrumented tests still requires a device or emulator. Swift's
Linux tests work here; Apple-platform builds still require macOS/Xcode.

No staff-only endpoints, credentials, registry logins, or release actions
are part of setup. Keep any credentials for real uploads or releases in
Amp secrets, not committed files or snapshot-time authentication. Follow
[RELEASING.md](RELEASING.md) for publishing.

To refresh dependencies after editing manifests, rerun `.agents/setup`
from the repository root. The npm cache key includes all workspace
manifests, the lockfile, and Node/npm versions. If `node_modules` was
manually damaged, remove `test-collector-javascript/node_modules/.orb-dependencies`
before rerunning setup to force a clean install. Other collectors use
their package managers' normal cache checks. To verify lifecycle changes,
run `bash -n .agents/setup .agents/resume`, time setup twice, and check
tool availability in a new login shell rather than sourcing the profile.
