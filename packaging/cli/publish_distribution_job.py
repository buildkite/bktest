"""Dispatch the supported stable distribution routes from the approved archive."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from urllib.error import HTTPError
from urllib.request import urlopen

from publish import open_stage
from release_input import approved_input


def files(args, verify):
    archive, _, sha, _, tag, _, source = args
    build = os.environ['BUILDKITE_BUILD_ID']
    steps = []
    with open_stage(Path(archive), sha, tag, source) as stage, tempfile.TemporaryDirectory() as tmp:
        for name, asset in sorted(stage.assets.items()):
            if not name.startswith('files/'):
                continue
            basename = asset.path.name
            url = f'https://packages.buildkite.com/buildkite/bktec/files/{basename}'
            try:
                with urlopen(url, timeout=60) as response:
                    if hashlib.file_digest(response, 'sha256').hexdigest() != asset.sha256:
                        raise ValueError(f'published Files digest conflict: {basename}')
            except HTTPError as error:
                if error.code != 404 or verify:
                    raise
                path = f'dist/cli-distribution/dist/buildkite-packages/{basename}'
                matches = subprocess.check_output(['buildkite-agent', 'artifact', 'search', '--build', build,
                                                   '--format', r'%j %p\n', path], text=True).splitlines()
                approved = subprocess.check_output(['buildkite-agent', 'artifact', 'search', '--build', build,
                                                    '--step', 'cli-distribution-inputs', '--format', r'%j %p\n',
                                                    path], text=True).splitlines()
                if len(matches) != 1 or matches != approved:
                    raise ValueError(f'expected one approved Files artifact: {path}')
                subprocess.run(['buildkite-agent', 'artifact', 'download', '--build', build, path, tmp], check=True)
                with (Path(tmp) / path).open('rb') as retained:
                    if hashlib.file_digest(retained, 'sha256').hexdigest() != asset.sha256:
                        raise ValueError(f'retained Files digest conflict: {path}')
                steps.append({'label': f'Publish {basename}', 'key': f'cli-files-{len(steps)}',
                              'concurrency': 1, 'concurrency_group': 'bktest-release/cli-files',
                              'plugins': [{'publish-to-packages#v2.2.0': {
                                  'registry': 'buildkite/bktec', 'artifact_build_id': build, 'artifacts': path}}]})
    if steps:
        subprocess.run(['buildkite-agent', 'pipeline', 'upload'], input=json.dumps({'steps': steps}),
                       text=True, check=True)


def main():
    with approved_input() as args:
        phase = sys.argv[1]
        if phase == 'inputs':
            Path('dist').mkdir(exist_ok=True)
            subprocess.run([sys.executable, '-B', 'packaging/cli/prepare_publish.py', *args,
                            'dist/cli-distribution'], check=True)
        elif phase in ('files', 'verify-files'):
            files(args, verify=phase == 'verify-files')
        elif phase == 'native':
            # An operator checks absence using the signed catalog and authorizes
            # one missing package; the endpoint has no dry-run/idempotency key.
            package, fmt, arch = sys.argv[2:]
            confirmed = subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-native-missing'],
                                                text=True).split()
            if f'{package}-{fmt}-{arch}' in confirmed:
                subprocess.run([sys.executable, '-B', 'packaging/cli/publish_native.py', *args,
                                '--package', package, '--format', fmt, '--arch', arch, '--confirmed-missing'],
                               check=True)
        else:
            raise ValueError(f'unknown release phase: {phase}')


if __name__ == '__main__':
    main()
