"""Run a release phase with the archive pinned by this build's stage receipt."""

from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from uuid import UUID

from publish import validate_ref


@contextmanager
def approved_input(*, stable=True):
    tag = os.environ['BUILDKITE_TAG']
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    validate_ref(tag, source, mode='stable' if stable else None)
    build = str(UUID(os.environ['BUILDKITE_BUILD_ID']))
    receipt_path = 'dist/cli-release-receipt.json'
    artifacts = ['buildkite-agent', 'artifact']
    scope = ['--build', build, '--include-retried-jobs']
    matches = subprocess.check_output([
        *artifacts, 'search', *scope, '--step', 'cli-stage', '--format', r'%j %p\n', receipt_path,
    ], text=True).splitlines()
    # A retry must not silently replace the receipt the release owner reviewed.
    if len(matches) != 1 or matches[0].split(' ', 1)[1:] != [receipt_path]:
        raise ValueError(f'expected exactly one {receipt_path} from cli-stage')
    job = str(UUID(matches[0].split(' ', 1)[0]))
    path = 'dist/cli-release.tar'
    with tempfile.TemporaryDirectory() as directory:
        subprocess.run([*artifacts, 'download', *scope, '--step', job, receipt_path, directory], check=True)
        receipt = json.loads((Path(directory) / receipt_path).read_text())
        if (receipt['build_id'], receipt['stage_job_id'], receipt['tag'], receipt['source']) != (build, job, tag, source):
            raise ValueError('stage receipt does not match this build, job, tag and checkout')
        sha = receipt['archive_sha256']
        if not re.fullmatch('[0-9a-f]{64}', sha):
            raise ValueError('invalid stage receipt archive SHA-256')
        subprocess.run([*artifacts, 'download', *scope, '--step', job, path, directory], check=True)
        yield (str(Path(directory) / path), '--archive-sha256', sha, '--tag', tag, '--source', source)
