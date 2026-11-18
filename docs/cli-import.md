# Test Engine CLI history import

This is the initial review import, not the source freeze or a release. The old repository remains authoritative until cutover. Before cutover, record the frozen upstream `main` SHA and verify any delta separately.

| Ref or object | SHA |
| --- | --- |
| `buildkite/bktest` `origin/main` at import | `44d95e2f4579234be7da82fc43709f8e929a39e5` |
| `buildkite/test-engine-client` `origin/main` imported | `9cef50ba76acaca0725a46c76e23753c5851b222` |
| Rewritten CLI `main` | `678126ec172dd4a9261df245fd0bf4796d2bff9b` |
| First splice, with the destination base first and rewritten source second | `4e3ca55d4e2854c7e5fb916f22a77fa89748635d` |
| Source root tree and splice's `bktest/` tree | `b8c9e0fb61691628aee0b431b85f466ecb3a0341` |
| Layout commit | `d7fd6933b5afed687e8bd6f0f5f44377afb8ed24` |
| Help and version branding commit | `27f87e18d47872a4a31f35c22a3716a7f1fc3427` |

The rewrite carries 2,067 commits reachable from upstream `main`. A recursive `git ls-tree -r -z` comparison matched all 284 entries by path, object ID and mode before the layout change. No bare issue/PR references remained in the rewritten commit messages. Rewritten commits have new IDs and do not retain upstream signatures. The original commits and tags remain intact in `buildkite/test-engine-client`.

## Layout and review refs

- `go.mod` and `go.sum` are at the root. The module is `github.com/buildkite/bktest/v3` (Go 1.27.3); the only CLI entry point is `./cmd/bktest`. The old root CLI tests moved with it. Implementation packages remain under `internal/`.
- The CLI's `docs/`, `CHANGELOG.md`, MIT `LICENSE.txt`, `packaging/`, `bin/` and `util/supported_features/` are at the root. The README retains the collectors' documentation alongside the imported CLI guide.
- All eight collector trees and the root `.agents/`, `.github/` and `.buildkite/` trees match the destination base. The old CLI Buildkite and GoReleaser definitions are available at `4e3ca55d4:bktest/` for the CI and distribution follow-up; they are not installed at active paths.
- `refs/heads/te-8012-bktest-cli-import:refs/heads/te-8012-bktest-cli-import` is the only review refspec. Use `--no-follow-tags`. The 115 rewritten `bktec-history/v*` tags exist only in the isolated rewrite, not in the destination. No non-default branch, tag, or release from the old repository is included in the review push.

## Repeat the rewrite and splice a delta

Use the collector import method with `git-filter-repo` 2.47.0. From a new scratch directory, fetch into a bare repo and rewrite a separate fresh clone. Do not filter either working repository or reuse a previously filtered clone.

```sh
git init --bare -b main source.git
git -C source.git fetch --no-tags https://github.com/buildkite/test-engine-client.git \
  refs/heads/main:refs/heads/main 'refs/tags/v*:refs/tags/v*'
# Record and review this exact source SHA before rewriting.
git -C source.git rev-parse main
git clone --no-local source.git rewritten
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
  git -C rewritten filter-repo \
  --to-subdirectory-filter bktest \
  --tag-rename ':bktec-history/' \
  --message-callback 'return re.sub(br"(?<![\w/])#(\d+)\b", b"buildkite/test-engine-client#\\1", message)'
test "$(git -C source.git rev-parse 'main^{tree}')" = \
  "$(git -C rewritten rev-parse HEAD:bktest)"
```

In the destination, use a disposable worktree at the **latest import splice before the layout**, not the laid-out branch tip. Set `rewrite` to the absolute path of the fresh rewritten clone. The initial splice used `--allow-unrelated-histories`; a delta must not need it.

```sh
previous=$(git rev-parse HEAD^2)
git fetch --no-tags "$rewrite" HEAD
next=$(git rev-parse FETCH_HEAD)
git merge-base --is-ancestor "$previous" "$next" || exit 1
git log --oneline "$previous..$next"
# If next equals previous, there is no CLI delta to merge.
git merge --no-ff -m 'Merge test-engine-client delta into bktest/' "$next"
test "$(git rev-parse HEAD:bktest)" = "$(git -C "$rewrite" rev-parse HEAD:bktest)"
```

Record the new source SHA, rewritten tip and splice, then replay the layout and branding commits on the updated splice and rerun validation. Reconcile import metadata with the new pins rather than treating this initial import manifest as the final cutover manifest. Do not force-push the review branch to replace published history with a rehearsal.

The offline rehearsal added one synthetic commit to upstream `main` (`80558c1dd450f426ba8f40b4c58cdd16188986e4`). Its rewritten tip (`b7b93047af76fcb641ab36a4092a6d9b28fa388c`) had the original rewritten tip as its direct parent and qualified `#123`. A normal two-parent merge preserved both lineages and matched the synthetic source tree. Replaying the layout and branding left only the synthetic change in `internal/version/version.go`. No rehearsal refs were published.
