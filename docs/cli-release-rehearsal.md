# CLI CI and release rehearsal

The public `bktest` pipeline routes Go changes to lint, the full Go test suite
with race detection and coverage, Ruby runner integration, and six cross-builds.
Ruby- and Python-only collector changes run their own CLI fixture jobs as well
as their collector pipelines. The CLI fixtures use these checked-out collectors
and local fake servers, not the old client's Test Engine wrappers. Live service
tests are opt-in and outside both pipelines; keep their tokens out of public CI.

## Stage and verify locally

From the repository root, with the Go version in `go.mod`, Python 3.11+ and
nFPM 2.49.0 on `PATH`:

```sh
bash packaging/cli/stage-checkout.sh dist/cli-release
python3 -B -m unittest discover -s packaging/cli -v
```

The untagged build uses `3.0.0-rehearsal`. A tagged build requires an exact
`v3` SemVer tag, a clean checkout, and the peeled tag commit equal to `HEAD`.
The stager writes six CGO-disabled payloads once and then copies or packages
them. The manifest records the version, source commit and SHA-256 inventory.
Keep the stage for a retry: never rebuild a missing upload from a different
checkout or reselect the version in the compatibility repository.

| Stage | Intended destination after approval |
| --- | --- |
| `github/bktest/` and `github/test-engine-client/` | Both GitHub releases; each has its own names and checksums and both get `manifest.json`. Retain the old release's direct DEB/RPM downloads. Set explicit target commits: canonical source and the generated compatibility commit respectively. |
| `files/` | Buildkite Packages `buildkite/bktec`, using its exact versioned `.bin`/`.exe`/checksum names. Not a stable channel update. |
| `packages/deb/` and `packages/rpm/` | `test-engine-client-deb` and `test-engine-client-rpm`, retaining package `bktec` with both commands. |
| `image/linux_{amd64,arm64}/` | Build contexts for `buildkite/test-engine-client`, `public.ecr.aws/buildkite/test-engine-client`, and `packages.buildkite.com/buildkite/test-engine-client-docker/test-engine-client`. Both commands are installed; keep `/usr/local/bin/bktec` as the entrypoint. Build local OCI images from the contexts before checking a container off. |
| `tap/` | Replace `Formula/bktec.rb` in `buildkite/homebrew-buildkite` and add `Aliases/bktest -> ../Formula/bktec.rb`. Keep `bktec` the sole owning formula. |

The staging job uploads only a Buildkite artifact (`dist/cli-release.tar`).
For a local release simulation, `packaging/cli/rehearse.py` takes the stage, a
fake destination directory, and explicit `--tag`, `--source` and `--approve`
arguments. It cannot contact a registry. It reserves the version with the
manifest, refuses mismatched bytes, and fills in missing files only. Both
GitHub releases are simulated for prereleases; the stage still contains
Files and packages, but no prerelease registry publication is enabled. A
pre-existing fake stable pointer is untouched, even with a staged Files archive.

## Proposed cutover (not enabled by this change)

1. In `buildkite/saas-iac` `public-tf/pipelines.tf`, retain
   `pipeline_bktest` on `Open Source`, with its existing branch/PR/fork checks
   and authorization block. Set `archived = true` on
   `pipeline_test_engine_client_release` and disable its branch, tag, PR and
   fork triggers (`build_branches`, `build_tags`, `build_pull_requests`,
   `build_pull_request_forks` all `false`). Keep the pipeline and build history.
   Verify no in-flight, blocked, or retryable legacy job can still publish
   before authorizing a new producer. Do not unblock or cancel any job here.
2. Keep the mirror's existing PR, CI, and no-force-push protections in
   `public-tf/repos.tf` (`repo_test_engine_client`). For the generated mirror,
   protect `refs/heads/main` and `refs/tags/v*` from arbitrary writes; restrict
   updates to the approved compatibility writer/release owners. Check an
   approved writer can fast-forward without granting broad bypass and that
   the required legacy CI status (`buildkite/test-engine-client`) still has a
   producer. Disable automatic CI only for unintended generated refs, not the
   source verification needed to update `main`. Do not rely on the old
   `test-splitter` redirect without verifying that status on a compatibility PR.
3. In `pipeline_bktest_release` (same `public-tf/pipelines.tf`), keep `Open
   Source Deploy`, tag events on, PR/forks off. After the old producer has
   stopped, change `branch_configuration` from `main test-collector-*/v*` to
   `main test-collector-*/v* v*`. This is a trigger filter, not authorization.
   Have a release owner select an unused version and an approved canonical
   source commit. Verify `build.tag`, not the branch name, and require the
   matching manifest and tag commit. Do not route `bktec-history/v*`,
   collector tags, or untagged builds to the CLI publisher.
4. A future publish step needs a deployment-team-only `allowed_teams` block
   after staging, with an explicit dependency on the block and
   `RELEASE_PUBLISH=true` on that step only. Keep credentials off the stager.
   Scope credential trust to the `buildkite/bktest-release` pipeline, the
   approved `refs/tags/v3.*` scope, and the publish step key (e.g. `cli-publish`)
   using OIDC subject/claims where supported. In `public-tf/registries.tf`,
   move the `bktec` Files writer policy off `test-engine-client-release`,
   without granting both producers simultaneous write access. Verify the Docker,
   DEB/RPM, GitHub and tap permissions independently; none is enabled here.
5. Test the first authorized prerelease by exact version. Do not move either
   GitHub `latest`, the formula, Docker `latest` at any of the three registries,
   or any stable package channel. Require separate verification before
   enabling prereleases on a package registry; GitHub prerelease settings do
   not gate other destinations. For a separately approved stable release,
   finish and verify all versioned writes before moving mutable pointers.
   Retry only a missing identical artifact and stop on a differing hash.
