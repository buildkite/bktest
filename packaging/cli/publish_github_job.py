"""Tag the reviewed legacy merge and publish paired GitHub releases."""

import os
import subprocess
import sys

from publish import validate_ref
from release_input import approved_input


def main():
    env = os.environ
    if (env.get('CLI_RELEASE_GITHUB_ENABLED') != 'true' or
            (env.get('BUILDKITE_ORGANIZATION_SLUG'), env.get('BUILDKITE_PIPELINE_SLUG'),
             env.get('BUILDKITE_SOURCE'), env.get('BUILDKITE_STEP_KEY')) !=
            ('buildkite', 'bktest-release', 'webhook', 'cli-publish-github')):
        raise ValueError('requires the enabled GitHub publish step on a canonical release tag webhook')
    if not env.get('GH_TOKEN'):
        raise ValueError('the release publishing credential is not available to this job')
    subprocess.run([sys.executable, '-B', 'packaging/cli/sync_legacy.py', 'tag'], check=True)
    legacy = subprocess.check_output(['buildkite-agent', 'meta-data', 'get', 'cli-legacy-source'], text=True).strip()
    with approved_input(stable=False) as args:
        _, _, _, _, tag, _, source = args
        mode = 'prerelease' if '-' in tag else 'stable'
        validate_ref(tag, source, legacy, mode=mode)
        subprocess.run([sys.executable, '-B', 'packaging/cli/publish.py', *args,
                        '--legacy-source', legacy, '--mode', mode, '--apply'], check=True)


if __name__ == '__main__':
    main()
