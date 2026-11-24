"""Promote the verified Homebrew formula, preserving its legacy alias."""

import base64
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
from urllib.request import urlopen

from publish import open_stage
from release_input import approved_input
from sync_legacy import git

TAP = 'https://github.com/buildkite/homebrew-buildkite.git'


def main():
    with approved_input() as (archive, _, sha, _, tag, _, source), tempfile.TemporaryDirectory() as scratch:
        with open_stage(Path(archive), sha, tag, source) as stage:
            # The generated formula points to these legacy GitHub assets.
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
        current = formula.read_bytes()
        if (alias.exists() or alias.is_symlink()) and (not alias.is_symlink() or os.readlink(alias) != '../Formula/bktec.rb'):
            raise ValueError('unexpected current bktest alias')
        if current == content and alias.is_symlink():
            return
        # Bind promotion to the tap head checked by the release approver.
        approved = subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-tap-head'], text=True).strip()
        if git(repo, 'rev-parse', 'HEAD') != approved:
            raise ValueError('tap head moved since approval')
        formula.write_bytes(content)
        if not alias.is_symlink():
            alias.symlink_to('../Formula/bktec.rb')
        git(repo, 'add', 'Formula/bktec.rb', 'Aliases/bktest')
        git(repo, '-c', 'user.name=Buildkite CLI Release', '-c', 'user.email=release@buildkite.com',
            'commit', '-m', f'Update bktec to {tag}')
        token = os.environ['CLI_TAP_TOKEN']
        auth = dict(os.environ, GIT_CONFIG_COUNT='1', GIT_CONFIG_KEY_0='http.https://github.com/.extraheader',
                    GIT_CONFIG_VALUE_0='Authorization: Basic ' + base64.b64encode(f'x-access-token:{token}'.encode()).decode())
        git(repo, 'push', TAP, 'HEAD:refs/heads/master', env=auth)
        remote = git(repo, 'ls-remote', TAP, 'refs/heads/master')
        if remote != f'{git(repo, "rev-parse", "HEAD")}\trefs/heads/master':
            raise ValueError('tap head readback differs after push')


if __name__ == '__main__':
    main()
