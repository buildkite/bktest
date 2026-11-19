#!/usr/bin/env python3
"""Manually reconcile the two versioned GitHub releases from an approved stage tar.

Both protected tags must already exist. No tag or mutable channel is changed.
Serialize runs for a version and retain the original archive for all retries.
"""

import argparse
from contextlib import contextmanager
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import tarfile
import tempfile
from typing import Protocol

from stage import TARGETS, VERSION, digest


@dataclass(frozen=True)
class Asset:
    path: Path
    sha256: str


@dataclass(frozen=True)
class Stage:
    version: str
    source: str
    archive_sha256: str
    assets: dict[str, Asset]


class Destination(Protocol):
    name: str

    def preflight(self, stage: Stage) -> tuple[str, ...]:
        """Read only: reject unsafe state, otherwise return exact missing names."""
        ...

    def publish_missing(self, stage: Stage, missing: tuple[str, ...]) -> None:
        """Fill only missing names. Never replace an object or move a channel."""
        ...


def validate_ref(tag, *sources, mode=None):
    if not re.fullmatch('v' + VERSION, tag) or not all(re.fullmatch(r'[a-f0-9]{40}', sha) for sha in sources):
        raise ValueError('expected an exact v3 CLI tag and full source SHAs')
    if mode is not None and mode != ('prerelease' if '-' in tag else 'stable'):
        raise ValueError('release mode does not match the approved tag')


@contextmanager
def open_stage(archive, archive_sha256, tag, source):
    """Validate and unpack only the known stage members into private scratch space."""
    validate_ref(tag, source)
    version = tag[1:]
    with tempfile.TemporaryDirectory(prefix='bktest-release-') as tmp, archive.open('rb') as data:
        if hashlib.file_digest(data, 'sha256').hexdigest() != archive_sha256:
            raise ValueError('stage archive SHA-256 differs from the approved digest')
        data.seek(0)

        checksums = {
            f'github/bktest/bktest_{version}_checksums.txt': [],
            f'github/test-engine-client/bktec_{version}_checksums.txt': [],
            f'files/bktec-checksums-{version}.txt': [],
        }
        legacy_sum = f'github/test-engine-client/bktec_{version}_checksums.txt'
        aliases = []
        for system, arch in TARGETS:
            ext = '.exe' if system == 'windows' else ''
            canonical = f'github/bktest/bktest_{version}_{system}_{arch}{ext}'
            legacy = f'github/test-engine-client/bktec_{version}_{system}_{arch}{ext}'
            files = f'files/bktec-{system}-{arch}-{version}{ext or ".bin"}'
            for checksum, name in zip(checksums, (canonical, legacy, files)):
                checksums[checksum].append(name)
            aliases.append((canonical, legacy, files))
            if system == 'linux':
                image = f'image/linux_{arch}'
                aliases.append((canonical, f'{image}/bktest', f'{image}/bktec'))
                for fmt in ('deb', 'rpm'):
                    checksums[legacy_sum].append(f'{legacy}.{fmt}')
                    aliases.append((f'{legacy}.{fmt}', f'packages/{fmt}/{Path(legacy).name}.{fmt}'))

        expected = {'manifest.json', 'tap/Formula/bktec.rb', *checksums,
                    *(f'image/linux_{arch}/Dockerfile' for arch in ('amd64', 'arm64')),
                    *(name for pair in aliases for name in pair)}
        symlinks = {'tap/Aliases/bktest': '../Formula/bktec.rb'}
        directories = {str(p) for name in expected | symlinks.keys() for p in PurePosixPath(name).parents}
        seen = set()
        assets = {}
        with tarfile.open(fileobj=data, mode='r:*') as tar:
            for member in tar:
                name = member.name.removeprefix('./') or '.'
                if name in seen:
                    raise ValueError(f'duplicate stage member: {name}')
                seen.add(name)
                if member.isdir() and name in directories:
                    continue
                if member.issym() and symlinks.get(name) == member.linkname:
                    continue  # Validate the tap alias, but never extract it.
                if name not in expected or not member.isreg() or not member.size:
                    raise ValueError(f'unexpected stage member: {name}')
                path = Path(tmp) / name
                path.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(member) as src, path.open('xb') as dst:
                    shutil.copyfileobj(src, dst)
                assets[name] = Asset(path, digest(path))
        if assets.keys() != expected or not symlinks.keys() <= seen:
            raise ValueError('incomplete stage archive inventory')

        manifest = json.loads(assets['manifest.json'].path.read_text())
        if manifest != dict(version=version, source=source, symlinks=symlinks,
                            sha256={name: asset.sha256 for name, asset in assets.items() if name != 'manifest.json'}):
            raise ValueError('stage manifest differs from approved source, version or extracted inventory')
        for paths in aliases:
            if len({assets[name].sha256 for name in paths}) != 1:
                raise ValueError(f'staged aliases differ: {", ".join(paths)}')
        for checksum, paths in checksums.items():
            lines = [f'{assets[name].sha256}  {Path(name).name}' for name in paths]
            if sorted(assets[checksum].path.read_text().splitlines()) != sorted(lines):
                raise ValueError(f'checksum inventory differs: {checksum}')
        yield Stage(version, source, archive_sha256, assets)


def reconcile(stage: Stage, github: tuple[Destination, ...], *,
              stable_destinations: tuple[Destination, ...] = (), apply: bool = False):
    if '-' in stage.version and stable_destinations:
        raise ValueError('prereleases cannot use stable registry destinations')
    if sorted(destination.name for destination in github) != ['github/buildkite/bktest', 'github/buildkite/test-engine-client']:
        raise ValueError('both canonical and legacy GitHub destinations are required')
    destinations = (*github, *stable_destinations)
    pending = [(destination, destination.preflight(stage)) for destination in destinations]
    for destination, missing in pending:
        print(f'{destination.name}: {", ".join(missing) if missing else "complete"}')
    if apply:
        for destination, missing in pending:
            if missing:
                destination.publish_missing(stage, missing)
        for destination in destinations:
            missing = destination.preflight(stage)
            if missing:
                raise ValueError(f'{destination.name}: still missing {", ".join(missing)}')
        print('Selected versioned destinations verified')


def main():
    from publish_github import GitHubRelease

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path, help='retained dist/cli-release.tar')
    parser.add_argument('--archive-sha256', required=True, help='independently approved SHA-256 of the archive')
    parser.add_argument('--tag', required=True, help='exact approved version tag (already exists in both repos)')
    parser.add_argument('--source', required=True, help='full canonical commit SHA')
    parser.add_argument('--legacy-source', required=True, help='full generated legacy commit SHA')
    parser.add_argument('--mode', required=True, choices=('stable', 'prerelease'), help='approved release policy')
    parser.add_argument('--apply', action='store_true', help='enable writes after separate authorization; default is read-only')
    args = parser.parse_args()
    try:
        validate_ref(args.tag, args.source, args.legacy_source, mode=args.mode)
        token = os.environ.get('GH_TOKEN')
        if not token:
            raise ValueError('GH_TOKEN is required to inspect both repositories, including draft releases')
        with open_stage(args.archive, args.archive_sha256, args.tag, args.source) as stage:
            destinations = tuple(GitHubRelease(repo, args.legacy_source, token)
                                 for repo in ('bktest', 'test-engine-client'))
            reconcile(stage, destinations, apply=args.apply)
    except (OSError, ValueError, tarfile.TarError) as error:
        parser.exit(1, f'{error}\n')


if __name__ == '__main__':
    main()
