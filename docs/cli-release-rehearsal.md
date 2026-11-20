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
The stager ignores ambient Go workspaces and builds the six CGO-disabled
payloads using the checkout's module in read-only mode, then copies or packages
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

The staging job uploads `dist/cli-release.tar`. A separate, credential-free
job downloads it from the staging job, verifies the members, and uploads
`dist/cli-release-receipt.json` with the build and staging job IDs, tag, source,
and archive and manifest SHA-256s. A receipt with a null tag is a rehearsal,
not a release input. Record the exact receipt before approving any handoff.
For a local release simulation, `packaging/cli/rehearse.py` takes the stage, a
fake destination directory, and explicit `--tag`, `--source` and `--approve`
arguments. It cannot contact a registry. It reserves the version with the
manifest, refuses mismatched bytes, and fills in missing files only. Resume is
supported between completed file copies, not mid-copy. If a copy is interrupted,
the simulator rejects the partial file as a hash conflict. Remove the confirmed
partial file from the fake destination before retrying with the same stage. If
the reservation manifest itself is partial, reset that fake version directory.
Both GitHub releases are simulated for prereleases; the stage still contains
Files and packages, but no prerelease registry publication is enabled. A
pre-existing fake stable pointer is untouched, even with a staged Files archive.

## Hand off an approved stable stage to the existing legacy uploaders

This is a manual runbook, not a pipeline. Local preparation needs no credentials.
**The upload steps are not runnable with the pipelines as configured.** The
existing Files jobs depend on the legacy `release` job, which runs GoReleaser,
and the canonical stage has no writer credentials. Before the legacy writer is
retired, the release owner must identify and separately approve a way to run
only the handoff jobs under an authorized legacy release identity, without the
normal bootstrap. Confirm artifact access and each destination's permissions
from that execution context. If no such path is available, stop after local
preparation; do not unblock or reuse the normal release job as a workaround.

Even with that path, do not upload until the release owner authorizes the
specific version and destinations, verifies both exact-version GitHub releases,
and confirms no other writer can publish this version. Do not run
`prepare-release.sh`, `release.sh`, `svu`, or GoReleaser; none should select a
version, tag a commit, or generate replacement payloads.

### Select and verify the artifact

Record the approved canonical source SHA, tag, archive SHA-256, `bktest-release`
build UUID and staging job UUID. On a clean scratch directory in an authorized
artifact-reading job, fetch the exact build and job, not the latest build or an
artifact wildcard. Do not include artifacts from retried jobs. For a local
rehearsal, use the already-retained archive instead of calling the agent:

```sh
buildkite-agent artifact download 'dist/cli-release.tar' "$scratch" \
  --build "${STAGE_BUILD_ID:?}" --step "${STAGE_JOB_ID:?}"
```

From the canonical checkout, set `ARCHIVE` to that file, and `ARCHIVE_SHA256`,
`TAG`, and `SOURCE` to the independently approved values. Set `OUT` to a new
directory under scratch. The landed verifier checks the digest, source, version,
all members and checksums before any files are copied. This command never calls
Buildkite or a registry:

```sh
PYTHONPATH=packaging/cli python3 -B - \
  "${ARCHIVE:?}" "${ARCHIVE_SHA256:?}" "${TAG:?}" "${SOURCE:?}" "${OUT:?}" <<'PY'
from pathlib import Path
from shutil import copyfile
import sys

from publish import open_stage, validate_ref

archive, sha, tag, source, out = sys.argv[1:]
validate_ref(tag, source, mode="stable")
with open_stage(Path(archive), sha, tag, source) as stage:
    root = Path(out)
    root.mkdir(mode=0o700)
    for name, asset in sorted(stage.assets.items()):
        if not name.startswith(("files/", "packages/", "image/")):
            continue
        path = root / ("dist/buildkite-packages/" + asset.path.name if name.startswith("files/") else name)
        path.parent.mkdir(parents=True, exist_ok=True)
        copyfile(asset.path, path)
        if name.startswith("image/") and path.name in ("bktec", "bktest"):
            path.chmod(0o755)
        print(f"{asset.sha256}  {path.relative_to(root)}")
PY
```

Retain that digest output with the approval. It covers seven Files artifacts,
four packages and two three-file image contexts. Never regenerate the checksum
file or packages from the compatibility checkout.

### Files and native packages (writes require approval)

- **Files:** Use the existing `publish-to-packages#v2.2.0` plugin, registry
  `buildkite/bktec`, and one job per exact filename under
  `dist/buildkite-packages/` (six binaries and one checksum). First retain the
  seven files as Buildkite artifacts in an approved handoff build. Search that
  build for each exact path and confirm one matching artifact from the approved
  handoff job. Independently download each exact path from that build into an
  empty directory and compare SHA-256 with the verifier output before releasing
  the upload jobs. The plugin accepts `artifact_build_id` but not a job selector
  or a digest. Freeze the artifact set: no competing jobs, retries or duplicate
  paths may change what that build-level lookup returns. With `file` set to one
  approved relative path, `expected_sha` from the receipt, and `verify_dir` an
  empty scratch directory, check the same build-level lookup the plugin uses:

  ```sh
  buildkite-agent artifact search "$file" --build "$HANDOFF_BUILD_ID" --format '%i %j %p\n'
  # Continue only if the output has exactly one artifact, from the approved job.
  buildkite-agent artifact download "$file" "$verify_dir" --build "$HANDOFF_BUILD_ID" &&
    (cd "$verify_dir" && printf '%s  %s\n' "$expected_sha" "$file" | sha256sum --check --strict)
  ```

  Only after that passes, use the existing plugin configuration (substitute
  literal approved values):

  ```yaml
  plugins:
    - publish-to-packages#v2.2.0:
        registry: buildkite/bktec
        artifact_build_id: APPROVED_HANDOFF_BUILD_UUID
        artifacts: dist/buildkite-packages/EXACT_VERSIONED_FILENAME
  ```

  Its OIDC audience is `https://packages.buildkite.com/buildkite/bktec`. The
  current Files policy grants `write_packages` to issuer
  `https://agent.buildkite.com` with organization `buildkite` and pipeline
  `test-engine-client-release`, not to the canonical stage job. Read back from
  `https://packages.buildkite.com/buildkite/bktec/files/<exact filename>` and
  compare bytes by SHA-256, including the checksum file itself.
- **DEB/RPM:** Use the unchanged legacy `.buildkite/steps/upload-linux-packages.sh`
  once per approved `packages/deb/bktec_<version>_linux_<arch>.deb` or
  `packages/rpm/bktec_<version>_linux_<arch>.rpm`, from a verified legacy
  checkout with the reviewed uploader. Check the file SHA-256 against
  the receipt immediately before passing its absolute path to the script, for
  example `sh "$legacy_checkout/.buildkite/steps/upload-linux-packages.sh" "$file"`.
  Keep the package name `bktec` and existing registries `test-engine-client-deb`
  and `test-engine-client-rpm`. The script requests a 180-second agent OIDC token
  for `https://packages.buildkite.com/buildkite/<registry>` and POSTs the file to
  `/v2/packages/organizations/buildkite/registries/<registry>/packages` on
  `api.buildkite.com`. Verify `write_packages` for the actual legacy release job
  against both existing policies; access to Files does not imply native access.
  Each accepted package can become selectable by unpinned APT/DNF as its index
  updates, independently per architecture and format. Authorize that exposure
  **before** the first native POST, and check both arches in the signed indexes.

After an ambiguous POST, stop that destination and record the exact file and
response. Do not retry the plugin job or script blindly. Verify the exact
filename/version/architecture and SHA-256 via inventory and served bytes, or
have the registry owner confirm absence after propagation. Skip a verified match;
resume only a confirmed missing file with the same approved bytes. Stop on a
conflict or unverifiable state; this runbook does not replace or delete objects.

### Images (build before authorizing push)

Build each platform once from its verified context on a prepared builder. The
Dockerfile preserves `/usr/local/bin/bktec` as entrypoint and installs both
names. Record the resolved Alpine base digest for each platform and the local
image IDs; retain the built images for retries. Building the image can fetch the
base and Alpine packages, but must not log in to or push to any destination:

```sh
for arch in amd64 arm64; do
  local_ref="bktec-stage:$TAG-$arch"
  docker buildx build --platform "linux/$arch" --load --provenance=false --sbom=false \
    --tag "$local_ref" "$OUT/image/linux_$arch" || exit
  docker image inspect --format '{{.Id}}' "$local_ref" > "$OUT/image-$arch.iid" || exit
  docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}} {{json .Config.Entrypoint}}' \
    "$local_ref" || exit
done
```

Stop unless the reported platforms are `linux/amd64` and `linux/arm64`, each
with `[/usr/local/bin/bktec]` as its sole entrypoint. Retain the built images
and ID files together (use `docker image save` if handing off to another host).

After separate approval, use the existing registry login identities from the
legacy release job. Do not put credentials on the builder or in the receipt:

| Destination repository | Existing login and permission to verify |
| --- | --- |
| `docker.io/buildkite/test-engine-client` | `DOCKERHUB_USER` / `DOCKERHUB_PASSWORD` via the `pipeline-buildkite-test-engine-client-release` SSM role in `445615400570`; verify the account can push to this repository. |
| `public.ecr.aws/buildkite/test-engine-client` | Existing `pipeline-buildkite-test-engine-client-release` web-identity role in `172840064832`; `aws ecr-public get-login-password --region us-east-1`, Docker user `AWS`; verify public ECR login and push permissions for this repository. |
| `packages.buildkite.com/buildkite/test-engine-client-docker/test-engine-client` | 300-second Buildkite OIDC token for `https://packages.buildkite.com/buildkite/test-engine-client-docker`, Docker user `buildkite`; verify this registry grants the executing legacy release job package-write access. |

For each approved repository (`repo`), check exact existing version and arch
tags before pushing. Skip only an exact match to a previously recorded digest;
a differing or ambiguous tag stops the route. For each confirmed missing
alias, set `arch` to its architecture and run:

```sh
alias_ref="$repo:$TAG-$arch"
docker image tag "$(cat "$OUT/image-$arch.iid")" "$alias_ref" &&
docker image push "$alias_ref" &&
docker buildx imagetools inspect "$alias_ref"
```

Record the **registry manifest digest** from each push and confirm it in exact
tag readback (the local image ID is not the manifest digest). Using those two
read-back digests, publish only the versioned multi-arch list if missing:

```sh
docker manifest create "$repo:$TAG" "$repo@$amd64_digest" "$repo@$arm64_digest" &&
docker manifest push --purge "$repo:$TAG" &&
docker buildx imagetools inspect "$repo:$TAG"
```

Capture the list digest and verify its Linux amd64/arm64 child digests equal the
alias digests. Stop after any uncertain write and re-read the exact references
before authorizing a retry; do not rebuild. Keep `latest` unchanged at all three
registries. This handoff does not select a version, change a tag policy or move
a GitHub release or Homebrew pointer.

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
