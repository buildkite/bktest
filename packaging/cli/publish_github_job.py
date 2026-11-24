"""Publish paired GitHub releases after the restricted release block."""

import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from uuid import UUID

from publish import validate_ref


def approved(field):
    return subprocess.check_output(['buildkite-agent', 'meta-data', 'get', field], text=True).strip()


def main():
    env = os.environ
    if (env.get('CLI_RELEASE_GITHUB_ENABLED') != 'true' or
            (env.get('BUILDKITE_ORGANIZATION_SLUG'), env.get('BUILDKITE_PIPELINE_SLUG'),
             env.get('BUILDKITE_SOURCE'), env.get('BUILDKITE_STEP_KEY')) !=
            ('buildkite', 'bktest-release', 'webhook', 'cli-publish-github')):
        raise ValueError('requires the enabled GitHub publish step on a canonical release tag webhook')
    tag = env.get('BUILDKITE_TAG', '')
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    validate_ref(tag, source)
    if not env.get('GH_TOKEN'):
        raise ValueError('the release publishing credential is not available to this job')
    mode = 'prerelease' if '-' in tag else 'stable'
    legacy = approved('cli-legacy-source')
    validate_ref(tag, source, legacy, mode=mode)
    archive_sha = approved('cli-archive-sha256')
    if not re.fullmatch(r'[0-9a-f]{64}', archive_sha):
        raise ValueError('expected the approved archive SHA-256')
    stage_job = str(UUID(approved('cli-stage-job')))
    build = str(UUID(env['BUILDKITE_BUILD_ID']))

    fetch = ['git', 'fetch', '--no-tags']
    if subprocess.check_output(['git', 'rev-parse', '--is-shallow-repository'], text=True).strip() == 'true':
        fetch.append('--unshallow')
    subprocess.run([*fetch, 'https://github.com/buildkite/bktest.git',
                    'refs/heads/main'], check=True)
    subprocess.run(['git', 'merge-base', '--is-ancestor', source, 'FETCH_HEAD'], check=True)
    path = 'dist/cli-release.tar'
    with tempfile.TemporaryDirectory() as scratch:
        subprocess.run(['buildkite-agent', 'artifact', 'download', '--build', build, '--step',
                        stage_job, path, scratch], check=True)
        subprocess.run([sys.executable, '-B', 'packaging/cli/publish.py', str(Path(scratch) / path),
                        '--archive-sha256', archive_sha, '--tag', tag, '--source', source,
                        '--legacy-source', legacy, '--mode', mode, '--apply'], check=True)


if __name__ == '__main__':
    main()
