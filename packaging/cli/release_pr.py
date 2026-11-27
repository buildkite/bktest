"""Prepare and verify the reviewed PR handoffs in a CLI release build."""

import base64
import json
import os
import re
import subprocess
from urllib.parse import urlencode
from urllib.request import Request, urlopen
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
        self.tag = tag
        self.branch = f'bktest-release/{tag}-{UUID(os.environ["BUILDKITE_BUILD_ID"])}'
        self.url = f'https://github.com/buildkite/{name}'
        self.pulls_url = f'https://api.github.com/repos/buildkite/{name}/pulls'
        self.prepared_key = f'cli-{name}-prepared'

    def prepare(self, repo):
        head = git(repo, 'rev-parse', 'HEAD')
        if head == git(repo, 'rev-parse', f'refs/heads/{self.base}'):
            message = f'{self.name}:{self.base} already matches. Continue without a PR.'
            prepared = f'none:{head}'
        else:
            ref = f'refs/heads/{self.branch}'
            existing = git(repo, 'for-each-ref', '--format=%(objectname)', f'refs/remotes/origin/{self.branch}')
            if existing:
                git(repo, 'diff', '--quiet', existing, head)
                head = existing
            else:
                git(repo, 'push', self.url + '.git', f'{head}:{ref}', env=git_auth())
            url = self.open_pull_request(head)
            message = f'Review and merge: [{self.name} #{url.rsplit("/", 1)[-1]}]({url})'
            prepared = head
        print(message)
        subprocess.run(['buildkite-agent', 'meta-data', 'set', self.prepared_key, prepared], check=True)
        subprocess.run(['buildkite-agent', 'annotate', message, '--context', self.prepared_key], check=True)

    def pull_requests(self, state):
        # The handoff repositories are public, so lookups don't need credentials.
        query = urlencode({'state': state, 'base': self.base, 'head': f'buildkite:{self.branch}'})
        with urlopen(f'{self.pulls_url}?{query}', timeout=30) as response:
            return json.load(response)

    def matches(self, pr, head):
        return (pr['base']['ref'] == self.base and pr['head']['ref'] == self.branch and pr['head']['sha'] == head and
                (pr['head']['repo'] or {}).get('full_name') == f'buildkite/{self.name}')

    def open_pull_request(self, head):
        # Reuse the prepared PR on retry; don't replace a closed, unmerged PR.
        pulls = self.pull_requests('all')
        if pulls:
            if len(pulls) != 1 or not (pulls[0]['state'] == 'open' or pulls[0]['merged_at']):
                raise ValueError(f'{self.name}: expected at most one open or merged release PR')
            pr = pulls[0]
        else:
            body = {'title': f'Update for CLI release {self.tag}', 'head': self.branch, 'base': self.base,
                    'body': f'Prepared by the CLI release build for `{self.tag}`. After review, CI and merge, unblock the release build.'}
            request = Request(self.pulls_url, data=json.dumps(body).encode(), headers={
                'Authorization': f'Bearer {os.environ["GH_TOKEN"]}',
                'Accept': 'application/vnd.github+json', 'Content-Type': 'application/json',
            })
            with urlopen(request, timeout=30) as response:
                pr = json.load(response)
        if not self.matches(pr, head):
            raise ValueError(f'{self.name}: release PR does not match the prepared branch and commit')
        return f'{self.url}/pull/{pr["number"]}'

    def merged_commit(self, repo):
        prepared = metadata(self.prepared_key)
        if prepared.startswith('none:'):
            commit = prepared.removeprefix('none:')
        else:
            merges = [pr for pr in self.pull_requests('closed') if pr['merged_at'] and self.matches(pr, prepared)]
            if len(merges) != 1:
                raise ValueError(f'{self.name}: expected the prepared release branch merged into {self.base}')
            commit = merges[0]['merge_commit_sha']
        if not re.fullmatch(r'[0-9a-f]{40}', commit or ''):
            raise ValueError(f'{self.name}: expected a full merged commit SHA')
        git(repo, 'merge-base', '--is-ancestor', commit, f'refs/heads/{self.base}')
        return commit
