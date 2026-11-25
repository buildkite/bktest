"""Prepare the Homebrew formula PR or verify its promotion to master."""

import argparse
import hashlib
import os
from pathlib import Path
import tempfile
from urllib.request import urlopen

from publish import open_stage
from release_input import approved_input
from release_pr import ReleasePR, git, metadata

TAP = 'https://github.com/buildkite/homebrew-buildkite.git'
ALIAS_TARGET = '../Formula/bktec.rb'


def matches(formula, alias, content):
    return (not formula.is_symlink() and formula.read_bytes() == content and
            alias.is_symlink() and os.readlink(alias) == ALIAS_TARGET)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'verify'])
    args = parser.parse_args()
    with approved_input() as (archive, _, sha, _, tag, _, source), tempfile.TemporaryDirectory() as scratch:
        with open_stage(Path(archive), sha, tag, source) as stage:
            # The generated formula points to these legacy GitHub assets.
            if args.action == 'prepare':
                for name, asset in stage.assets.items():
                    if not name.startswith('github/test-engine-client/') or asset.path.suffix in ('.deb', '.rpm', '.txt'):
                        continue
                    url = f'https://github.com/buildkite/test-engine-client/releases/download/{tag}/{asset.path.name}'
                    with urlopen(url, timeout=60) as response:
                        if hashlib.file_digest(response, 'sha256').hexdigest() != asset.sha256:
                            raise ValueError(f'GitHub bytes do not match the Homebrew input: {name}')
            content = stage.assets['tap/Formula/bktec.rb'].path.read_bytes()
        repo = Path(scratch) / 'tap'
        git(scratch, 'clone', '--branch', 'master', TAP, str(repo))
        formula = repo / 'Formula/bktec.rb'
        alias = repo / 'Aliases/bktest'
        pr = ReleasePR('homebrew-buildkite', 'master', tag)
        if args.action == 'verify':
            commit = pr.merged_commit(repo)
            if not matches(formula, alias, content):
                raise ValueError('Homebrew master does not serve the approved formula and alias')
            git(repo, 'checkout', '--detach', commit)
            if not matches(formula, alias, content):
                raise ValueError('merged Homebrew commit does not match the approved formula and alias')
            return
        if formula.is_symlink():
            raise ValueError('unexpected formula symlink')
        if (alias.exists() or alias.is_symlink()) and (not alias.is_symlink() or os.readlink(alias) != ALIAS_TARGET):
            raise ValueError('unexpected current bktest alias')
        if matches(formula, alias, content):
            pr.prepare(repo)
            return
        # Bind promotion to the tap head checked by the release approver.
        if git(repo, 'rev-parse', 'HEAD') != metadata('cli-tap-head'):
            raise ValueError('tap head moved since approval')
        git(repo, 'checkout', '--detach')
        formula.write_bytes(content)
        if not alias.is_symlink():
            alias.symlink_to(ALIAS_TARGET)
        git(repo, 'add', 'Formula/bktec.rb', 'Aliases/bktest')
        git(repo, '-c', 'user.name=Buildkite CLI Release', '-c', 'user.email=release@buildkite.com',
            'commit', '-m', f'Update bktec to {tag}')
        pr.prepare(repo)


if __name__ == '__main__':
    main()
