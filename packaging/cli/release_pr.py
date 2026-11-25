"""Prepare and verify the reviewed PR handoffs in a CLI release build."""

import base64
import json
import os
import re
import subprocess
from urllib.parse import urlencode
from urllib.request import urlopen
from uuid import UUID


def git(repo, *args, env=None):
    return subprocess.check_output(['git', '-C', repo, *args], env=env, text=True).strip()


def git_auth():
    token = os.environ['GH_TOKEN']
    if not token:
        raise ValueError('missing GitHub release credential')
    header = base64.b64encode(f'x-access-token:{token}'.encode()).decode()
    return dict(os.environ, GIT_CONFIG_COUNT='1', GIT_CONFIG_KEY_0='http.https://github.com/.extraheader',
                GIT_CONFIG_VALUE_0=f'Authorization: Basic {header}')


def metadata(key):
    return subprocess.check_output(['buildkite-agent', 'meta-data', 'get', key], text=True).strip()


class ReleasePR:
    def __init__(self, name, base, tag):
        self.name = name
        self.base = base
        self.branch = f'bktest-release/{tag}-{UUID(os.environ["BUILDKITE_BUILD_ID"])}'
        self.url = f'https://github.com/buildkite/{name}'
        self.prepared_key = f'cli-{name}-prepared'

    def prepare(self, repo):
        head = git(repo, 'rev-parse', 'HEAD')
        if head == git(repo, 'rev-parse', f'refs/heads/{self.base}'):
            print(f'{self.name}:{self.base} already matches. Continue without a PR.')
            prepared = f'none:{head}'
        else:
            ref = f'refs/heads/{self.branch}'
            existing = git(repo, 'for-each-ref', '--format=%(objectname)', f'refs/remotes/origin/{self.branch}')
            if existing:
                git(repo, 'diff', '--quiet', existing, head)
                head = existing
            else:
                git(repo, 'push', self.url + '.git', f'{head}:{ref}', env=git_auth())
            print(f'Open a PR: {self.url}/compare/{self.base}...{self.branch}?expand=1')
            prepared = head
        subprocess.run(['buildkite-agent', 'meta-data', 'set', self.prepared_key, prepared], check=True)

    def merged_commit(self, repo):
        prepared = metadata(self.prepared_key)
        if prepared.startswith('none:'):
            commit = prepared.removeprefix('none:')
        else:
            # Both handoff repositories are public; no Pull requests permission is needed to read a PR.
            query = urlencode({'state': 'closed', 'base': self.base, 'head': f'buildkite:{self.branch}'})
            with urlopen(f'https://api.github.com/repos/buildkite/{self.name}/pulls?{query}', timeout=30) as response:
                merges = [pr for pr in json.load(response) if pr['merged_at'] and pr['base']['ref'] == self.base and
                          pr['head']['ref'] == self.branch and pr['head']['sha'] == prepared and
                          (pr['head']['repo'] or {}).get('full_name') == f'buildkite/{self.name}']
            if len(merges) != 1:
                raise ValueError(f'{self.name}: expected the prepared release branch merged into {self.base}')
            commit = merges[0]['merge_commit_sha']
        if not re.fullmatch(r'[0-9a-f]{40}', commit or ''):
            raise ValueError(f'{self.name}: expected a full merged commit SHA')
        git(repo, 'merge-base', '--is-ancestor', commit, f'refs/heads/{self.base}')
        return commit
