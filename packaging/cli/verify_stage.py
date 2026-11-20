"""Read back the retained Buildkite stage and record what a release can approve."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
from uuid import UUID

from publish import open_stage
from stage import digest


def main():
    build_id = os.environ['BUILDKITE_BUILD_ID']
    path = 'dist/cli-release.tar'
    match = subprocess.check_output([
        'buildkite-agent', 'artifact', 'search', '--build', build_id,
        '--step', 'cli-stage', '--format', r'%j %p\n', path,
    ], text=True).splitlines()
    if len(match) != 1 or match[0].split(' ', 1)[1:] != [path]:
        raise ValueError(f'expected exactly one {path} from cli-stage')
    stage_job_id = str(UUID(match[0].split(' ', 1)[0]))
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    tag = os.environ.get('BUILDKITE_TAG') or None
    if tag and subprocess.check_output(
        ['git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}'], text=True,
    ).strip() != source:
        raise ValueError('release tag does not point at the verifier checkout')

    with tempfile.TemporaryDirectory() as scratch:
        subprocess.run([
            'buildkite-agent', 'artifact', 'download', '--build', build_id,
            '--step', stage_job_id, path, scratch,
        ], check=True)
        archive = Path(scratch) / path
        with open_stage(archive, digest(archive), tag or 'v3.0.0-rehearsal', source) as stage:
            receipt = dict(tag=tag, source=source, build_id=build_id, stage_job_id=stage_job_id,
                           archive_sha256=stage.archive_sha256,
                           manifest_sha256=stage.assets['manifest.json'].sha256)
    output = Path('dist/cli-release-receipt.json')
    output.parent.mkdir(exist_ok=True)
    data = json.dumps(receipt, indent=2) + '\n'
    output.write_text(data)
    print(data, end='')


if __name__ == '__main__':
    main()
