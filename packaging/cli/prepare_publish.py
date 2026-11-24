#!/usr/bin/env python3
"""Prepare verified Files, native packages, and OCI build contexts without publishing."""

import argparse
from pathlib import Path
from shutil import copyfile

from publish import open_stage, validate_ref


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('output', type=Path, help='new directory for publication inputs')
    parser.add_argument('--archive-sha256', required=True)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--source', required=True)
    args = parser.parse_args()
    validate_ref(args.tag, args.source, mode='stable')
    with open_stage(args.archive, args.archive_sha256, args.tag, args.source) as stage:
        args.output.mkdir(mode=0o700)
        for name, asset in sorted(stage.assets.items()):
            if not name.startswith(('files/', 'packages/', 'image/')):
                continue
            relative = Path('dist/buildkite-packages', Path(name).name) if name.startswith('files/') else Path(name)
            path = args.output / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            copyfile(asset.path, path)
            if name.startswith('image/') and path.name in ('bktec', 'bktest'):
                path.chmod(0o755)
            print(f'{asset.sha256}  {relative}')


if __name__ == '__main__':
    main()
