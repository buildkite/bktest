"""Verify the staged tarball and write its Buildkite release receipt."""

import json
import os
from pathlib import Path
import subprocess
from uuid import UUID

from publish import open_stage
from stage import digest


def main():
    build_id = str(UUID(os.environ['BUILDKITE_BUILD_ID']))
    stage_job_id = str(UUID(os.environ['BUILDKITE_JOB_ID']))
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    tag = os.environ.get('BUILDKITE_TAG') or None
    if tag and subprocess.check_output(
        ['git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}'], text=True,
    ).strip() != source:
        raise ValueError('release tag does not point at the verifier checkout')

    archive = Path('dist/cli-release.tar')
    with open_stage(archive, digest(archive), tag or 'v3.0.0-rehearsal', source) as stage:
        receipt = dict(tag=tag, source=source, build_id=build_id, stage_job_id=stage_job_id,
                       archive_sha256=stage.archive_sha256,
                       manifest_sha256=stage.assets['manifest.json'].sha256)
    output = Path('dist/cli-release-receipt.json')
    data = json.dumps(receipt, indent=2) + '\n'
    output.write_text(data)
    print(data, end='')


if __name__ == '__main__':
    main()
