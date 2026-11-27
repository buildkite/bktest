# Releasing the CLI

The [`bktest-release` pipeline](https://buildkite.com/buildkite/bktest-release)
publishes the CLI from a `v3.x.y` or `v3.x.y-rc.N` tag. It builds once and
publishes the same files under both names, `bktest` and `bktec`. The only
things you do by hand are merging two PRs and unblocking the build after each.
Unblocking needs the `test-engine` Buildkite team.

## Release candidate

1. Push the tag on the reviewed `main` commit:

   ```sh
   git tag v3.8.0-rc.1 <main-sha>
   git push origin v3.8.0-rc.1
   ```

2. The build opens a sync PR in `buildkite/test-engine-client`. The build
   summary links to it. Review and merge it, then unblock the build.
3. The build tags `test-engine-client` and publishes both GitHub prereleases.

An RC doesn't publish packages, images, or Homebrew, and doesn't move Latest.

## Stable release

1. Merge a PR that moves the `Unreleased` entries in `CHANGELOG.md` under a
   `## 3.8.0 - YYYY-MM-DD` heading.
2. Push `v3.8.0` on that merge commit. Don't retag an RC.
3. Merge the sync PR and unblock, as for an RC. The build then publishes both
   GitHub releases, Files, and DEB/RPM packages, and opens a formula PR in
   `buildkite/homebrew-buildkite`.
4. Merge the formula PR and unblock. The build pushes the images to Docker
   Hub, ECR Public, and Buildkite Packages, then moves `latest` and the
   legacy GitHub Latest.

If the sync or formula PR has no changes, the job says so. Unblock without
merging anything.

## When a job fails

Retry the failed job in the same build. Each publisher checks what already
exists and uploads only what's missing, so a retry is safe. Don't rebuild,
move a tag, or push a new tag to recover. If a retry keeps failing, check the
job log before changing anything by hand. Tags and published versions are
immutable.
