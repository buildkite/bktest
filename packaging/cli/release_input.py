"""Run a release phase with the archive pinned by the versioned approval block."""

from contextlib import contextmanager
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
    job = str(UUID(subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-stage-job'], text=True).strip()))
    sha = subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-archive-sha256'], text=True).strip()
    if not re.fullmatch('[0-9a-f]{64}', sha):
        raise ValueError('invalid approved archive SHA-256')
    path = 'dist/cli-release.tar'
    with tempfile.TemporaryDirectory() as directory:
        subprocess.run(['buildkite-agent', 'artifact', 'download', '--build', build, '--step', job,
                        path, directory], check=True)
        yield (str(Path(directory) / path), '--archive-sha256', sha, '--tag', tag, '--source', source)
