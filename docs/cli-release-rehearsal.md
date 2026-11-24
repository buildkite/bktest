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

## Automated tag workflow (disabled during cutover)

The route is disabled in the pipeline itself. The environment opt-in is
not a security boundary and cannot enable the route on its own.

Once enabled, push an approved, unused CLI tag pointing at the reviewed
canonical `main` commit. The build compiles and verifies once. At the first
`test-engine` block, enter the archive SHA-256 and exact stage job UUID from
the verifier receipt, never credentials. The sync job verifies the archive,
projects the CLI into `test-engine-client`, and atomically fast-forwards its
`main` and creates the same tag. It checks the projected bytes before the
push and reads back the tag. It will not force-push or move an existing tag.

The paired publisher uses that exact archive and verified legacy commit,
checks both refs and publishes both GitHub releases without setting Latest.
Stable builds then publish and read back the seven versioned Files and the
two OCI architecture aliases and multi-arch index in all three registries.
Prereleases stop after both GitHub releases. The legacy stable Go discovery
path can change as soon as its tag is created; approve that at the first block.

At the stable block, pin the current tap head, legacy GitHub Latest release
ID, and each registry's OCI Latest digest. List only missing native packages
after inspecting the signed catalogs. Once the native jobs complete, verify
the served hashes against the retained archive and confirm the readback block.
This releases tap and OCI Latest updates, followed by legacy GitHub Latest.
Canonical GitHub Latest does not move.

After an ambiguous
result, inspect both releases and read back the assets before retrying with the
same approval and retained archive. Follow `publish.py --help` if GitHub left an
empty upload placeholder. Do not rerun staging or move a tag to recover a write.

An existing tag built with staging-only YAML will never acquire a publisher
on retry. An approved recovery must use the existing build receipt and the
manual paired publisher with both existing tags, not move the canonical tag.

### Provision writer access before activation

Configure credentials manually for `bktest-release` in the approved secret
provider, injected only into the indicated job keys. Do not use shared pipeline
environment secrets. Require the canonical pipeline identity, protected CLI tag
ref, and exact job key at the credential provider; verify denials for staging,
rehearsals, collectors, and manual/API builds before enabling the group.

| Job key | Manual setup |
| --- | --- |
| `cli-sync-legacy` | `CLI_LEGACY_TOKEN`: short-lived GitHub App installation token with Contents write for `buildkite/test-engine-client` only. The repository and organization owners must review a narrow direct-update exception for the app on `main`; current required PR/CI rules do not permit this job. Retain non-fast-forward and deletion protection. |
| `cli-publish-github` | `GH_TOKEN`: GitHub App installation token with Contents write for only `buildkite/bktest` and `buildkite/test-engine-client`, with no tag or branch bypass. |
| `cli-promote-github` | `GH_TOKEN`: separate legacy-repo-only Contents write token for the approved promotion job, with no tag or branch bypass. |
| `cli-files-0` through `cli-files-6` | Files registry `buildkite/bktec`: job-scoped Buildkite OIDC `write_packages` for each Files plugin job, audience `https://packages.buildkite.com/buildkite/bktec`. |
| `cli-promote-native` | Buildkite OIDC `write_packages` on `test-engine-client-deb` and `test-engine-client-rpm`, with the exact registry URL as audience. Do not expose this grant to the verifier or OCI jobs. |
| `cli-publish-dockerhub`, `cli-promote-dockerhub` | `CLI_DOCKERHUB_USER` and repository-scoped `CLI_DOCKERHUB_TOKEN` for `buildkite/test-engine-client`. |
| `cli-publish-ecr`, `cli-promote-ecr` | Short-lived AWS role credentials with ECR Public login and push restricted to the existing `test-engine-client` repository. |
| `cli-publish-packages-oci`, `cli-promote-packages-oci` | OIDC write to `test-engine-client-docker` with audience `https://packages.buildkite.com/buildkite/test-engine-client-docker`. Configure `CLI_PACKAGES_OCI_CONTROL_TAG` and `CLI_PACKAGES_OCI_CONTROL_DIGEST` with an independently verified existing ref/digest. |
| `cli-promote-tap` | `CLI_TAP_TOKEN`: tap-only GitHub App installation token with Contents write for `buildkite/homebrew-buildkite`. The tap owners must review a narrow direct-update exception on `master`; the current protections must not be silently relaxed. |

Use separate creation and immutability rules for `v3.*` on both source
repositories: permit creation only for approved principals, and protect
updates/deletions without granting bypass of those protections. Verify all
legacy writer access is retired before transferring trust. Set up the
`cli-oci` queue with Python 3.11+, Buildx capable of exporting amd64 and arm64
OCI archives, Skopeo, and AWS CLI. Keep the `cli-build-oci` job credential-free.
Do not configure a queue-wide credential. Enable the group in a reviewed
change only after a no-public-write rehearsal
demonstrates the credential boundaries and the release owner accepts recovery.

## Prepare a stable stage for the existing destinations

Local preparation needs no credentials. The transports below record the
compatibility requirements; **the upload steps are not enabled in the canonical
pipeline**. The legacy pipeline is archived. Do not unblock or reuse its
historical jobs. Stop after preparation until the canonical write route and
its approval and credential policies have been verified.

Do not upload until the release owner authorizes the
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
directory under scratch. The command checks the digest, source, version,
all members and checksums before any files are copied. This command never calls
Buildkite or a registry:

```sh
python3 -B packaging/cli/prepare_publish.py "${ARCHIVE:?}" "${OUT:?}" \
  --archive-sha256 "${ARCHIVE_SHA256:?}" --tag "${TAG:?}" --source "${SOURCE:?}"
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
  retired pipeline's Files grant has been removed; do not reuse it. The
  canonical staging job must not have `write_packages`. Read back from
  `https://packages.buildkite.com/buildkite/bktec/files/<exact filename>` and
  compare bytes by SHA-256, including the checksum file itself.
- **DEB/RPM:** Run the canonical native handoff with one format and architecture:

  ```sh
  python3 -B packaging/cli/publish_native.py "$ARCHIVE" \
    --archive-sha256 "$ARCHIVE_SHA256" --tag "$TAG" --source "$SOURCE" \
    --format deb --arch arm64
  ```

  This verifies the full archive and reports the exact package and digest.
  Only add `--confirmed-missing` in an approved writer job after confirming
  the package is absent and approving exposure in the signed index. The command
  sends one POST and does not retry; verify the result separately, even on success.
  Keep the package name `bktec` and existing registries `test-engine-client-deb`
  and `test-engine-client-rpm`. The command uses the legacy endpoint and a 180-second agent OIDC token
  for `https://packages.buildkite.com/buildkite/<registry>` and POSTs the file to
  `/v2/packages/organizations/buildkite/registries/<registry>/packages` on
  `api.buildkite.com`. Verify `write_packages` for the approved native release job
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

After separate approval, use only the new grants for the approved canonical
publisher jobs. Do not put credentials on the builder or in the receipt:

| Destination repository | Existing login and permission to verify |
| --- | --- |
| `docker.io/buildkite/test-engine-client` | Repository-scoped Docker Hub writer; verify it can push only the intended repository. |
| `public.ecr.aws/buildkite/test-engine-client` | Scoped web-identity role; `aws ecr-public get-login-password --region us-east-1`, Docker user `AWS`; verify public ECR login and push permissions for this repository. |
| `packages.buildkite.com/buildkite/test-engine-client-docker/test-engine-client` | 300-second Buildkite OIDC token for `https://packages.buildkite.com/buildkite/test-engine-client-docker`, Docker user `buildkite`; verify this registry grants only the approved publishing job package-write access. |

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

## Cutover and operation

1. The legacy release pipeline was archived with its triggers disabled and
   its bootstrap retired. Its historical jobs are not recovery routes.
   Verify the remaining legacy package policies and credentials are revoked
   before granting a canonical writer. Ordinary legacy CI remains unchanged.
2. Keep the mirror's existing PR, CI, and no-force-push protections in
   `public-tf/repos.tf` (`repo_test_engine_client`). For the generated mirror,
   protect `refs/heads/main` and `refs/tags/v*` from arbitrary writes; restrict
   updates to the approved compatibility writer/release owners. Check an
   approved writer can fast-forward without granting broad bypass and that
   the required legacy CI status (`buildkite/test-engine-client`) still has a
   producer. Disable automatic CI only for unintended generated refs, not the
   source verification needed to update `main`. Do not rely on the old
   `test-splitter` redirect without verifying that status on a compatibility PR.
3. `bktest-release` admits `main test-collector-*/v* v3.*` on `Open Source
   Deploy`, with tag events on and PR/fork events off. This filter is not
   authorization to publish.
   Have a release owner select an unused version and an approved canonical
   source commit. Verify `build.tag`, not the branch name, and require the
   matching manifest and tag commit. Do not route `bktec-history/v*`,
   collector tags, or untagged builds to the CLI publisher.
4. Keep the credential boundary separate from the approval check. Scope
   writer trust to the `buildkite/bktest-release` pipeline, approved CLI
   tag refs and exact publish step using OIDC subject or claims where
   supported. Do not grant write permissions to staging or readback jobs.
   Verify Files, Docker, DEB/RPM, GitHub and tap permissions independently.
5. Test the first authorized prerelease by exact version. Do not move either
   GitHub `latest`, the formula, Docker `latest` at any of the three registries,
   or any stable package channel. Require separate verification before
   enabling prereleases on a package registry; GitHub prerelease settings do
   not gate other destinations. For a separately approved stable release,
   finish and verify all versioned writes before moving mutable pointers.
   Retry only a missing identical artifact and stop on a differing hash.
