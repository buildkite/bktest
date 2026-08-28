# Releasing collectors

This is the public release guide for every collector in this repository. It
describes the release contract without documenting credentials or private
infrastructure.

Anyone can prepare a release pull request. Pushing release tags, publishing
packages, approving release jobs, and creating GitHub releases require the
appropriate GitHub, Buildkite, or package-registry permissions. If you do not
have those permissions, prepare the pull request and ask a Buildkite Test
Engine maintainer to complete the release.

## Shared release contract

1. Prepare a focused pull request that updates the collector's version files
   and changelog, where it has one. Run that collector's tests and package
   build before merging.
2. Merge the release pull request to `main`.
3. An authorized maintainer tags the merge commit using
   `<collector-directory>/vX.Y.Z`, or a collector-supported prerelease form
   described below. Tags are scoped because all collectors share this
   repository; do not create an unprefixed release tag.

   ```sh
   git tag test-collector-python/v1.10.0 <release-merge-sha>
   git push origin test-collector-python/v1.10.0
   ```

4. Publish using the collector-specific method below. The tag version must
   exactly match the version in the collector's source.
5. Verify the version on its package registry, then create a
   [GitHub release](https://github.com/buildkite/bktest/releases/new) from the
   existing prefixed tag. Include a short summary and a link to the package.

Treat pushed release tags and published package versions as immutable. Never
put registry credentials in this repository or in release notes.

## Release status

- **Ruby:** `lib/buildkite/test_collector/version.rb`; RubyGems; Buildkite
  automation.
- **Python:** `pyproject.toml`; PyPI; Buildkite automation.
- **JavaScript:** `package.json`; npm; maintainer-operated.
- **Swift:** Git tag mirror; Swift Package Manager; Buildkite automation.
- **Elixir:** `mix.exs`; Hex; maintainer-operated.
- **.NET:** both package-project `.fsproj` files; NuGet;
  maintainer-operated.
- **Android:** `gradle.properties` and `RunEnvironment.VERSION_NAME`; Maven
  Central; Buildkite automation.
- **Rust:** `Cargo.toml`; crates.io; maintainer-operated.

Workflows inherited under individual collectors' `.github/workflows/`
directories are not active in this monorepo. GitHub only runs workflows from
the repository-level `.github/workflows/` directory.

## Buildkite-automated releases

Ruby, Python, Android, and Swift publish through the
[`bktest-release` pipeline](https://buildkite.com/buildkite/bktest-release).
The pipeline builds and validates release artifacts without publishing by
default.

Pushing the exact prefixed tag automatically creates a build for that tag
with a publish confirmation step. An authorized Buildkite maintainer then:

1. checks that the artifact build and version checks pass;
2. reviews and unblocks the publish step; and
3. confirms the package appears on the registry.

Untagged builds are safe rehearsals. If the publish confirmation step is
absent from a tag build, stop rather than trying to publish around the
pipeline: the tag does not exactly match `<collector-directory>/vX.Y.Z`.

### Ruby (RubyGems)

In the release pull request:

- update `lib/buildkite/test_collector/version.rb`;
- run `bundle` so `Gemfile.lock` remains current; and
- move the relevant entries from `Unreleased` into a versioned section in
  `CHANGELOG.md`.

Use a `test-collector-ruby/vX.Y.Z` tag and the automated process above.

### Python (PyPI)

In the release pull request, update `pyproject.toml` and run `uv lock` so
`uv.lock` contains the same version. A `[release]` pull request title does
not publish from this monorepo.

Use a `test-collector-python/vX.Y.Z` tag and the automated process above.

### Android (Maven Central)

In the release pull request, update both:

- `gradle.properties` (`VERSION_NAME`, retaining the `-SNAPSHOT` suffix); and
- `collector/test-data-uploader/src/main/kotlin/com/buildkite/test/collector/android/model/RunEnvironment.kt`
  (`VERSION_NAME`, without the suffix).

Update versioned usage examples when appropriate. Use a
`test-collector-android/vX.Y.Z` tag and the automated process above; the
pipeline removes `-SNAPSHOT` while building the release. Merges to `main` no
longer publish Android snapshots automatically.

### Swift (Swift Package Manager)

Swift Package Manager discovers versions from repository-level `vX.Y.Z`
tags, so the release pipeline mirrors Swift releases to the standalone
[`buildkite/test-collector-swift`](https://github.com/buildkite/test-collector-swift)
repository that existing consumers already use.

Swift supports stable `test-collector-swift/vX.Y.Z` tags and SemVer
prerelease tags such as `test-collector-swift/v2.0.0-beta.1`. It has no
version file to update. Use the automated process above. The pipeline extracts
`test-collector-swift/` from the tagged bktest history, checks that it extends
the existing release mirror, and pushes the corresponding plain version tag.
It never updates the standalone repository's default branch.

Do not create an unprefixed tag in bktest or push a release tag directly to
the standalone repository. After publishing, verify that the plain tag is
present there and resolves through Swift Package Manager.

## Maintainer-operated releases

For these collectors, an authorized registry maintainer publishes from a
clean checkout of the exact prefixed tag. Use the registry's normal secure
authentication; do not add tokens or fallback credentials to this repo.

### JavaScript (npm)

Use `npm version --no-git-tag-version X.Y.Z` in the release pull request so
`package.json` and `package-lock.json` stay in sync. After pushing
`test-collector-javascript/vX.Y.Z`, publish from
`test-collector-javascript/` with `npm publish`.

### Elixir (Hex)

Update `@version` in `mix.exs` and `CHANGELOG.md` in the release pull request.
Do not use `mix git_ops.release` to derive a version from repository-wide
history. After pushing `test_collector_elixir/vX.Y.Z`, publish from
`test_collector_elixir/` with `mix hex.publish`.

### .NET (NuGet)

Keep the `<Version>` in both package projects synchronized:

- `Buildkite.TestAnalytics.Common/Buildkite.TestAnalytics.Common.fsproj`
- `Buildkite.TestAnalytics.Xunit.reporters/Buildkite.TestAnalytics.Xunit.reporters.fsproj`

After pushing `test-collector-dotnet/vX.Y.Z`, run `dotnet pack
--configuration Release` from `test-collector-dotnet/` and publish both
packages to NuGet with the maintainer's authenticated tooling.

### Rust (crates.io)

Update `Cargo.toml` and `Cargo.lock` in the release pull request. Do not use a
tool that creates an unprefixed tag. After pushing
`test-collector-rust/vX.Y.Z`, run `cargo publish --dry-run` and then
`cargo publish` from `test-collector-rust/`.
