"""Build retained OCI layouts and copy verified tags without changing their digests."""

import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import build_opener, Request

from publish_github import NoRedirect
from release_input import approved_input

ARCHES = ('amd64', 'arm64')
INDEX = 'application/vnd.oci.image.index.v1+json'
MANIFEST = 'application/vnd.oci.image.manifest.v1+json'
PACKAGES_BACKEND = '0191e41f-89fd-7980-ae04-b883e5cbead2'
SKOPEO_IMAGE = 'quay.io/skopeo/stable@sha256:4531abd9632dca34af50bd524ec46db5bcaec01d3bb0c62dc9916a02b3472bb3'
REPOS = {
    'dockerhub': 'docker.io/buildkite/test-engine-client',
    'ecr': 'public.ecr.aws/buildkite/test-engine-client',
    'packages': 'packages.buildkite.com/buildkite/test-engine-client-docker/test-engine-client',
}


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def encode(data):
    return json.dumps(data, sort_keys=True, separators=(',', ':')).encode()


def layout(root, archives, tag):
    blobs = root / 'blobs/sha256'
    blobs.mkdir(parents=True)
    manifests = []
    for arch, archive in zip(ARCHES, archives, strict=True):
        with tarfile.open(archive) as tar:
            def blob(desc):
                sha = desc['digest']
                if not re.fullmatch(r'sha256:[0-9a-f]{64}', sha):
                    raise ValueError('invalid OCI blob digest')
                name = sha.split(':')[1]
                with tar.extractfile(f'blobs/sha256/{name}') as stream:
                    data = stream.read()
                if digest(data) != sha or len(data) != desc['size']:
                    raise ValueError('OCI blob differs from descriptor')
                (blobs / name).write_bytes(data)
                return data

            with tar.extractfile('index.json') as stream:
                descriptor, = json.load(stream)['manifests']
            manifest = json.loads(blob(descriptor))
            if 'manifests' in manifest:
                descriptor, = manifest['manifests']
                manifest = json.loads(blob(descriptor))
            config = json.loads(blob(manifest['config']))
            if (descriptor['mediaType'], config['os'], config['architecture'], config['config']['Entrypoint']) != (
                    MANIFEST, 'linux', arch, ['/usr/local/bin/bktec']):
                raise ValueError('OCI platform, manifest type or entrypoint differs')
            for layer in manifest['layers']:
                blob(layer)
            manifests.append({k: descriptor[k] for k in ('mediaType', 'digest', 'size')} |
                             {'platform': {'os': 'linux', 'architecture': arch}})
    index = encode({'schemaVersion': 2, 'mediaType': INDEX, 'manifests': manifests})
    sha = digest(index)
    (blobs / sha.split(':')[1]).write_bytes(index)
    refs = {f'{tag}-{arch}': desc for arch, desc in zip(ARCHES, manifests, strict=True)}
    refs[tag] = {'mediaType': INDEX, 'digest': sha, 'size': len(index)}
    (root / 'oci-layout').write_bytes(encode({'imageLayoutVersion': '1.0.0'}))
    (root / 'index.json').write_bytes(encode({'schemaVersion': 2, 'manifests': [
        desc | {'annotations': {'org.opencontainers.image.ref.name': ref}} for ref, desc in refs.items()
    ]}))
    return {ref: desc['digest'] for ref, desc in refs.items()}


def http(url, token=None):
    headers = {'Accept': ', '.join((INDEX, MANIFEST, 'application/vnd.docker.distribution.manifest.list.v2+json',
                                   'application/vnd.docker.distribution.manifest.v2+json')),
               'Cache-Control': 'no-cache'}
    if token:
        headers['Authorization'] = f'Bearer {token}'
    try:
        with build_opener(NoRedirect()).open(Request(url, headers=headers), timeout=45) as result:
            return result.status, result.read(2_000_000), result.headers.get('Link')
    except HTTPError as error:
        with error:
            return error.code, error.read(65536), None


def read_auth(target, repo):
    host, name = repo.split('/', 1)
    if target == 'dockerhub':
        url = 'https://auth.docker.io/token?' + urlencode({'service': 'registry.docker.io',
                                                          'scope': f'repository:{name}:pull'})
        host = 'registry-1.docker.io'
    elif target == 'ecr':
        url = 'https://public.ecr.aws/token/?service=public.ecr.aws&scope=aws'
    else:
        url = f'https://{host}/v2/auth?' + urlencode({'scope': f'repository:{name}:pull'})
    status, body, _ = http(url)
    if status != 200:
        raise ValueError(f'OCI read token request returned {status}')
    return f'https://{host}/v2/{name}', json.loads(body)['token']


def current(base, token, ref, target, control):
    status, body, _ = http(f'{base}/manifests/{ref}', token)
    if status == 200:
        return digest(body)
    if status == 404 and body:
        errors = json.loads(body).get('errors', [])
        if len(errors) == 1 and errors[0].get('code') == 'MANIFEST_UNKNOWN':
            return None
    if status == 404 and not body and target == 'packages' and ref not in ('latest', control[0]):
        known, pin = control
        if current(base, token, known, target, control) != pin:
            raise ValueError('Packages OCI control does not match')
        status, inventory, link = http(f'{base}/tags/list', token)
        if status == 200 and not link:
            catalog = json.loads(inventory)
            tags = catalog['tags']
            if (catalog.get('name') == PACKAGES_BACKEND and isinstance(tags, list)
                    and all(isinstance(tag, str) and tag for tag in tags)
                    and tags == sorted(set(tags)) and known in tags and ref not in tags):
                def suffix(cursor, expected):
                    query = urlencode({'last': cursor, 'n': 1})
                    status, body, link = http(f'{base}/tags/list?{query}', token)
                    return status == 200 and not link and json.loads(body) == (catalog | {'tags': expected})

                if suffix(known, [tag for tag in tags if tag > known]) and suffix(tags[-1], []):
                    return None
    raise ValueError(f'inconclusive OCI state: {target}:{ref} (HTTP {status})')


def upload(target, root, desired, tag, latest, control):
    repo = REPOS[target]
    if target == 'dockerhub':
        user, password = os.environ['CLI_DOCKERHUB_USER'], os.environ['CLI_DOCKERHUB_TOKEN']
    elif target == 'ecr':
        user = 'AWS'
        password = run('aws', 'ecr-public', 'get-login-password', '--region', 'us-east-1',
                       stdout=subprocess.PIPE, text=True).stdout.strip()
    else:
        user = 'buildkite'
        password = run('buildkite-agent', 'oidc', 'request-token', '--audience',
                       'https://packages.buildkite.com/buildkite/test-engine-client-docker', '--lifetime', '300',
                       stdout=subprocess.PIPE, text=True).stdout.strip()
    if not user or not password:
        raise ValueError(f'missing {target} OCI credentials')
    auth = root.parent / 'auth.json'
    auth.touch(mode=0o600, exist_ok=False)
    auth.write_bytes(encode({'auths': {repo.split('/')[0]: {'auth': base64.b64encode(
        f'{user}:{password}'.encode()).decode()}}}))
    base, token = read_auth(target, repo)
    state = {ref: current(base, token, ref, target, control) for ref in desired}
    if latest:
        if state != desired:
            raise ValueError('all versioned OCI refs must match before promotion')
        previous = run('buildkite-agent', 'meta-data', 'get', f'cli-previous-{target}',
                       stdout=subprocess.PIPE, text=True).stdout.strip()
        state = {'latest': current(base, token, 'latest', target, control)}
        if state['latest'] not in (previous, desired[tag]):
            raise ValueError(f'{target} latest differs from approved prior digest')
        pending = {'latest': tag}
    else:
        if any(value not in (None, desired[ref]) for ref, value in state.items()):
            raise ValueError(f'{target} versioned OCI conflict')
        pending = {ref: ref for ref in desired}
    for dst, src in pending.items():
        actual = current(base, token, dst, target, control)
        if actual == desired[src]:
            continue
        if actual != state[dst]:
            raise ValueError(f'{target}:{dst} moved after preflight')
        run('docker', 'run', '--rm',
            '--mount', f'type=bind,src={root},dst=/release/oci,readonly',
            '--mount', f'type=bind,src={auth},dst=/release/auth.json,readonly',
            SKOPEO_IMAGE, 'copy', '--all', '--preserve-digests', '--dest-authfile', '/release/auth.json',
            f'oci:/release/oci:{src}', f'docker://{repo}:{dst}')
        if current(base, token, dst, target, control) != desired[src]:
            raise ValueError(f'{target}:{dst} digest mismatch after publication')


def main():
    phase = sys.argv[1]
    if phase not in ('build', 'versioned', 'latest'):
        raise ValueError(f'unknown OCI phase: {phase}')
    with approved_input() as args, tempfile.TemporaryDirectory() as temp:
        tag = args[4]
        directory = Path(temp)
        output = Path('dist/cli-oci')
        if phase == 'build':
            inputs = directory / 'inputs'
            run(sys.executable, '-B', 'packaging/cli/prepare_publish.py', *args, inputs)
            output.mkdir(parents=True)
            for arch in ARCHES:
                run('docker', 'buildx', 'build', '--platform', f'linux/{arch}', '--provenance=false',
                    '--sbom=false', '--output', f'type=oci,dest={output}/linux_{arch}.tar', inputs / f'image/linux_{arch}')
            desired = layout(directory / 'oci', [output / f'linux_{arch}.tar' for arch in ARCHES], tag)
            (output / 'receipt.json').write_bytes(encode({'stage': args[2], 'refs': desired}))
            print(desired)
            return
        target = sys.argv[2]
        paths = ['receipt.json', *(f'linux_{arch}.tar' for arch in ARCHES)]
        for name in paths:
            run('buildkite-agent', 'artifact', 'download', f'{output}/{name}', temp,
                '--build', os.environ['BUILDKITE_BUILD_ID'], '--step', 'cli-build-oci')
        received = directory / output
        root = directory / 'oci'
        desired = layout(root, [received / name for name in paths[1:]], tag)
        if json.loads((received / paths[0]).read_bytes()) != {'stage': args[2], 'refs': desired}:
            raise ValueError('retained OCI layout differs from its receipt')
        control = (os.environ.get('CLI_PACKAGES_OCI_CONTROL_TAG'), os.environ.get('CLI_PACKAGES_OCI_CONTROL_DIGEST'))
        if target == 'packages' and (not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}', control[0] or '') or
                                     not re.fullmatch(r'sha256:[0-9a-f]{64}', control[1] or '')):
            raise ValueError('Packages OCI needs a known readable control ref and digest')
        upload(target, root, desired, tag, phase == 'latest', control)


if __name__ == '__main__':
    main()
