"""Build each platform once, then push the same images to all three registries."""

import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import build_opener, Request, urlopen

from promote_github import check_latest
from publish_github import NoRedirect
from release_input import approved_input

ARCHES = ('amd64', 'arm64')
PACKAGES = 'packages.buildkite.com/buildkite/test-engine-client-docker'
REPOS = ('docker.io/buildkite/test-engine-client', 'public.ecr.aws/buildkite/test-engine-client',
         f'{PACKAGES}/test-engine-client')
ACCEPT = ', '.join(f'application/vnd.{kind}+json' for kind in (
    'oci.image.index.v1', 'oci.image.manifest.v1', 'docker.distribution.manifest.list.v2',
    'docker.distribution.manifest.v2'))


def output(*command, **kwargs):
    return subprocess.check_output(command, text=True, **kwargs).strip()


def docker(*command, **kwargs):
    subprocess.run(['docker', *command], check=True, **kwargs)


def login(repo):
    host, name = repo.split('/', 1)
    scope = {'scope': f'repository:{name}:pull'}
    if host == 'public.ecr.aws':
        aws_env = os.environ | {key: os.environ[f'ECR_{key}'] for key in (
            'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN')}
        user = 'AWS'
        password = output('aws', 'ecr-public', 'get-login-password', '--region', 'us-east-1', env=aws_env)
        # Anonymous manifest reads are subject to a separate ECR rate limit.
        token = base64.b64encode(f'{user}:{password}'.encode()).decode()
    else:
        if host == 'docker.io':
            user, password = os.environ['CLI_DOCKERHUB_USER'], os.environ['CLI_DOCKERHUB_TOKEN']
            auth = 'https://auth.docker.io/token?' + urlencode(scope | {'service': 'registry.docker.io'})
            basic = base64.b64encode(f'{user}:{password}'.encode()).decode()
            auth = Request(auth, headers={'Authorization': f'Basic {basic}'})
        else:
            user = 'buildkite'
            password = output('buildkite-agent', 'oidc', 'request-token', '--audience',
                              f'https://{PACKAGES}', '--lifetime', '300')
            auth = f'https://{host}/v2/auth?' + urlencode(scope)
        with build_opener(NoRedirect()).open(auth, timeout=45) as response:
            token = json.load(response)['token']
    if not password or not token:
        raise ValueError(f'missing registry credential: {host}')
    docker('login', host, '--username', user, '--password-stdin', input=password + '\n', text=True)
    return token


def manifest(repo, ref, token, *, missing=False):
    host, name = repo.split('/', 1)
    if host == 'docker.io':
        host = 'registry-1.docker.io'
    request = Request(f'https://{host}/v2/{name}/manifests/{ref}', headers={
        'Accept': ACCEPT, 'Authorization': f'Bearer {token}', 'Cache-Control': 'no-cache',
    })
    try:
        with build_opener(NoRedirect()).open(request, timeout=45) as response:
            data = response.read()
    except HTTPError as error:
        with error:
            if missing and error.code == 404:
                body = error.read()
                if (not body and host == 'packages.buildkite.com') or (
                    body and [e['code'] for e in json.loads(body).get('errors', [])] == ['MANIFEST_UNKNOWN']
                ):
                    return None, None
        raise
    return 'sha256:' + hashlib.sha256(data).hexdigest(), json.loads(data)


def versions(repo, tag, token, images):
    # Verify a known tag first: Packages returns an empty 404 for absent tags.
    previous, _ = manifest(repo, 'latest', token)
    version_digest, _ = manifest(repo, tag, token, missing=True)
    pending = {}
    children = {}
    # A retry rebuilds the images, so keep tags an earlier attempt already pushed
    # instead of comparing them with this attempt's build.
    for arch, image in images.items():
        ref = f'{tag}-{arch}'
        children[arch], _ = manifest(repo, ref, token, missing=True)
        if children[arch] is None:
            pending[arch] = image
    if version_digest is not None:
        if pending:
            raise ValueError(f'{repo}:{tag} exists but its architecture tags are incomplete')
        verify_index(repo, tag, token, children)
        return previous, children
    for arch, image in pending.items():
        ref = f'{tag}-{arch}'
        docker('tag', image, f'{repo}:{ref}')
        docker('push', f'{repo}:{ref}')
        children[arch], existing = manifest(repo, ref, token)
        if existing['config']['digest'] != image:
            raise ValueError(f'{repo}:{ref} differs from the pushed image')
    create_index(repo, tag, children)
    verify_index(repo, tag, token, children)
    return previous, children


def create_index(repo, ref, children):
    docker('buildx', 'imagetools', 'create', '--tag', f'{repo}:{ref}',
           *(f'{repo}@{children[arch]}' for arch in ARCHES))


def verify_index(repo, ref, token, children):
    digest, index = manifest(repo, ref, token)
    platforms = [(item['platform']['os'], item['platform']['architecture'], item['digest'])
                 for item in index['manifests']]
    if sorted(platforms) != sorted(('linux', arch, sha) for arch, sha in children.items()):
        raise ValueError(f'{repo}:{ref} does not serve the two published platform images')
    print(f'{repo}:{ref} {digest}', flush=True)
    return digest


def main():
    with approved_input() as args, tempfile.TemporaryDirectory() as temp:
        subprocess.run([sys.executable, '-B', 'packaging/cli/prepare_publish.py', *args, f'{temp}/inputs'], check=True)
        tag = args[4]
        images = {}
        for arch in ARCHES:
            local = f'bktest-stage:{tag}-{arch}'
            docker('buildx', 'build', '--platform', f'linux/{arch}', '--load', '--provenance=false',
                   '--sbom=false', '--tag', local, f'{temp}/inputs/image/linux_{arch}')
            image, platform, entrypoint = output('docker', 'image', 'inspect', '--format',
                                                 '{{.Id}} {{.Os}}/{{.Architecture}} {{json .Config.Entrypoint}}',
                                                 local).split(maxsplit=2)
            if (platform != f'linux/{arch}' or not re.fullmatch(r'sha256:[0-9a-f]{64}', image)
                    or json.loads(entrypoint) != ['/usr/local/bin/bktec']):
                raise ValueError(f'expected a local linux/{arch} image')
            images[arch] = image

        # Use an isolated Docker login store only after both images are built.
        config = Path(temp) / 'docker'
        config.mkdir(mode=0o700)
        os.environ['DOCKER_CONFIG'] = str(config)
        published = {repo: versions(repo, tag, login(repo), images) for repo in REPOS}
        with urlopen('https://api.github.com/repos/buildkite/test-engine-client/releases/latest', timeout=45) as latest:
            check_latest(json.load(latest)['tag_name'], tag)
        for repo, (previous, refs) in published.items():
            token = login(repo)  # Refresh short-lived Packages credentials before promotion.
            expected = verify_index(repo, tag, token, refs)
            current, _ = manifest(repo, 'latest', token)
            if current == expected:
                continue
            if current != previous:
                raise ValueError(f'{repo}:latest changed during publication')
            docker('buildx', 'imagetools', 'create', '--tag', f'{repo}:latest', f'{repo}@{expected}')
            if verify_index(repo, 'latest', token, refs) != expected:
                raise ValueError(f'{repo}:latest digest differs from {repo}:{tag}')


if __name__ == '__main__':
    main()
