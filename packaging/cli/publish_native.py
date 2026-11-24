#!/usr/bin/env python3
"""Prepare or upload one approved stable DEB/RPM asset from a retained archive."""

import argparse
from pathlib import Path
import subprocess

from publish import open_stage, validate_ref


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('--archive-sha256', required=True)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--format', required=True, choices=('deb', 'rpm'))
    parser.add_argument('--arch', required=True, choices=('amd64', 'arm64'))
    parser.add_argument('--confirmed-missing', action='store_true',
                        help='make one write after approval of signed-index exposure and exact absence')
    args = parser.parse_args()
    validate_ref(args.tag, args.source, mode='stable')
    fmt = args.format
    with open_stage(args.archive, args.archive_sha256, args.tag, args.source) as stage:
        name = f'bktec_{stage.version}_linux_{args.arch}.{fmt}'
        asset = stage.assets[f'packages/{fmt}/{name}']
        print(f'{name} sha256:{asset.sha256}', flush=True)
        if not args.confirmed_missing:
            return
        audience = f'https://packages.buildkite.com/buildkite/test-engine-client-{fmt}'
        token = subprocess.check_output(['buildkite-agent', 'oidc', 'request-token',
                                         '--audience', audience, '--lifetime', '180'], text=True).strip()
        if not token or any(c.isspace() for c in token):
            parser.exit(1, 'No valid agent OIDC token; no upload attempted.\n')
        # Read the curl header from stdin, not a process argument or log.
        result = subprocess.run(['curl', '-q', '--silent', '--show-error', '--fail', '--output', '/dev/null',
                                 '--write-out', '%{http_code}',
                                 '--request', 'POST', '--header', '@-', '--form', f'file=@{asset.path}',
                                 f'https://api.buildkite.com/v2/packages/organizations/buildkite/'
                                 f'registries/test-engine-client-{fmt}/packages'],
                                input=f'Authorization: Bearer {token}\n', text=True, stdout=subprocess.PIPE)
        print('Read back exact served bytes and the signed index before another write; do not blindly retry.')
        if result.returncode or not result.stdout.startswith('2'):
            parser.exit(1, 'Native upload did not confirm success.\n')


if __name__ == '__main__':
    main()
