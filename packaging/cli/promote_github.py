"""Move legacy GitHub Latest only after verifying the complete paired release."""

import json
import os
from pathlib import Path
import subprocess

from publish import open_stage
from publish_github import GitHubRelease
from release_input import approved_input


def main():
    with approved_input() as (archive, _, sha, _, tag, _, source):
        legacy = subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-legacy-source'], text=True).strip()
        previous = subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-github-previous-latest'],
                                           text=True).strip()
        token = os.environ['GH_TOKEN']
        with open_stage(Path(archive), sha, tag, source) as stage:
            for repo in ('bktest', 'test-engine-client'):
                destination = GitHubRelease(repo, legacy, token)
                release, missing = destination.inspect(stage)
                if not release or release['draft'] or missing:
                    raise ValueError(f'{repo}: versioned release not complete')
            current = destination.get('/releases/latest')['id']
            if current == release['id']:
                return
            if str(current) != previous:
                raise ValueError('legacy GitHub Latest differs from the approval')
            with destination.request(f'{destination.api}{destination.prefix}/releases/{release["id"]}',
                                     method='PATCH', data=json.dumps({'make_latest': 'true'}).encode(),
                                     headers={'Content-Type': 'application/json'}):
                pass
            if destination.get('/releases/latest')['id'] != release['id']:
                raise ValueError('legacy GitHub Latest differs after promotion')


if __name__ == '__main__':
    main()
