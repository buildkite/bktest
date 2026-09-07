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
