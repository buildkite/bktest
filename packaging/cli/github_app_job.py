"""Mint a repository-scoped installation token before starting a CLI writer."""

import base64
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
from urllib.request import Request, build_opener

from publish_github import NoRedirect


JOBS = {
    'cli-sync-legacy': ('CLI_LEGACY_TOKEN', ['test-engine-client'], ['sync_legacy.py', '--apply']),
    'cli-publish-github': ('GH_TOKEN', ['bktest', 'test-engine-client'], ['publish_github_job.py']),
    'cli-promote-github': ('GH_TOKEN', ['test-engine-client'], ['promote_github.py']),
    'cli-promote-tap': ('CLI_TAP_TOKEN', ['homebrew-buildkite'], ['promote_tap.py']),
}


def jwt_part(data):
    return base64.urlsafe_b64encode(data).rstrip(b'=')


def installation_token(app_id, installation, pem, repositories):
    if not all(re.fullmatch(r'[1-9][0-9]*', value) for value in (app_id, installation)):
        raise ValueError('expected numeric GitHub App and installation IDs')
    now = int(time.time())
    claims = json.dumps({'iat': now - 60, 'exp': now + 540, 'iss': app_id}).encode()
    message = jwt_part(b'{"alg":"RS256","typ":"JWT"}') + b'.' + jwt_part(claims)
    with tempfile.TemporaryDirectory() as scratch:
        key = Path(scratch) / 'key.pem'
        key.write_bytes(pem)
        signature = subprocess.check_output(['openssl', 'dgst', '-sha256', '-sign', key], input=message)
    jwt = (message + b'.' + jwt_part(signature)).decode()
    request = Request(f'https://api.github.com/app/installations/{installation}/access_tokens', method='POST',
                      data=json.dumps({'repositories': repositories, 'permissions': {'contents': 'write'}}).encode(),
                      headers={'Authorization': f'Bearer {jwt}', 'Content-Type': 'application/json',
                               'Accept': 'application/vnd.github+json'})
    with build_opener(NoRedirect()).open(request, timeout=30) as response:
        token = json.load(response)['token']
    if not token or any(char.isspace() for char in token):
        raise ValueError('GitHub did not return a usable installation token')
    return token


def main():
    variable, repositories, (script, *args) = JOBS[os.environ['BUILDKITE_STEP_KEY']]
    app_id = os.environ.pop('CLI_GITHUB_APP_ID')
    installation = os.environ.pop('CLI_GITHUB_INSTALLATION_ID')
    pem = base64.b64decode(os.environ.pop('CLI_GITHUB_PRIVATE_KEY_B64'), validate=True)
    os.environ[variable] = installation_token(app_id, installation, pem, repositories)
    os.execv(sys.executable, [sys.executable, '-B', f'packaging/cli/{script}', *args])


if __name__ == '__main__':
    main()
