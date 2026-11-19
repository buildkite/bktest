#!/usr/bin/env python3
"""Plan or reconcile exact-version destinations from one approved stage tar.

Both protected tags must already exist. No tag or mutable channel is changed.
Serialize runs for a version and retain the original archive for all retries.
GitHub is always included. Files and OCI can be planned against their real
destinations, but registry apply (including DEB/RPM) is loopback rehearsal
only. Start the local fixture servers and CLI in the same loopback-only
network namespace; fixture credentials are fixed dummy values.

Recovery after a GitHub upload returns HTTP 502:
  1. Stop writes and rerun without --apply using the same approved inputs.
     If GitHub left no asset, normal missing-only resume is possible once
     both repositories pass preflight. If an empty starter is reported,
     record the repository, tag, draft ID, asset name and asset ID.
  2. Before any cleanup, independently recheck the peeled tag, draft ID,
     stage reservation (archive digest and both source SHAs), and asset
     membership: the ID must still identify the expected name on that exact
     owned draft, with state 'starter' and size 0. Stop if anything differs
     or ownership is unclear. This does not apply to published releases,
     uploaded assets, or hash conflicts.
  3. Only with separate approval for that exact asset ID, the serialized
     release owner may remove that one placeholder outside the driver.
     Rerun read-only preflight for both repositories with the same archive;
     resume only after preflight passes and writes are authorized again.
     The driver never deletes anything.
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
import socket
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


def loopback_origin(port):
    if not 1 <= port <= 65535:
        raise ValueError('expected a loopback port between 1 and 65535')
    return f'http://127.0.0.1:{port}'


def select_destinations(args):
    from publish_files import FilesDestination
    from publish_github import GitHubRelease
    from publish_linux_packages import LinuxPackagesDestination
    from publish_oci import EXISTING_REPOSITORIES, OCIDestination

    ports = (args.loopback_github, args.loopback_files, args.loopback_oci, args.loopback_linux_packages)
    local = args.loopback_github is not None
    if any(port is not None for port in ports) and not local:
        raise ValueError('loopback registry endpoints require --loopback-github')
    if local and (args.files != (args.loopback_files is not None) or
                  bool(args.oci) != (args.loopback_oci is not None)):
        raise ValueError('each selected Files or OCI loopback destination requires its matching port')
    if args.oci:
        if len(set(args.oci)) != len(args.oci):
            raise ValueError('select each OCI route only once')
        if args.oci_layout is None or not re.fullmatch(r'sha256:[0-9a-f]{64}', args.oci_index_sha256 or ''):
            raise ValueError('OCI routes require an approved layout and --oci-index-sha256')
    elif args.oci_layout is not None or args.oci_index_sha256 is not None:
        raise ValueError('OCI inputs require an explicit --oci route')
    registries = args.files or args.oci or args.loopback_linux_packages is not None
    if registries and args.mode != 'stable':
        raise ValueError('prereleases cannot use stable registry destinations')
    if registries and args.apply and not local:
        raise ValueError('real registry writes are disabled; omit --apply to plan, or select GitHub only')

    github_options = {}
    if local:
        if {name for _, name in socket.if_nameindex()} != {'lo'}:
            raise ValueError('loopback rehearsal requires a network namespace with only lo')
        origin = loopback_origin(args.loopback_github)
        github_options = dict(api=origin, uploads=origin)
        token = 'loopback-fixture-only'
    else:
        token = os.environ.get('GH_TOKEN')
        if not token:
            raise ValueError('GH_TOKEN is required to inspect both repositories, including draft releases')

    stable = []
    if args.files:
        if local:
            origin = loopback_origin(args.loopback_files)
            stable.append(FilesDestination('mock-read', 'mock-write' if args.apply else None,
                                           api_origin=origin, download_origin=origin, verified_create_only=True))
        else:
            read_token = os.environ.get('BUILDKITE_PACKAGES_READ_TOKEN')
            if not read_token:
                raise ValueError('BUILDKITE_PACKAGES_READ_TOKEN is required for the Files plan')
            stable.append(FilesDestination(read_token))
    if args.loopback_linux_packages is not None:
        stable.append(LinuxPackagesDestination(loopback_origin(args.loopback_linux_packages), {
            'https://packages.buildkite.com/buildkite/test-engine-client-deb': 'fake-deb',
            'https://packages.buildkite.com/buildkite/test-engine-client-rpm': 'fake-rpm',
        }.__getitem__))
    for route in args.oci:
        name = route
        if local:
            host = loopback_origin(args.loopback_oci).removeprefix('http://')
            name = f'{host}/{route.split("/", 1)[1]}'
        stable.append(OCIDestination(name, args.oci_layout, args.oci_index_sha256, args.archive_sha256))
    # Docker Hub and Public ECR share a legacy path; they need separate fixture
    # runs if both are to be rehearsed with different registry state.
    if len({destination.name for destination in stable}) != len(stable):
        raise ValueError('selected routes share a loopback repository; rehearse them separately')
    github = tuple(GitHubRelease(repo, args.legacy_source, token, **github_options)
                   for repo in ('bktest', 'test-engine-client'))
    return github, tuple(stable)


def main():
    from publish_oci import EXISTING_REPOSITORIES

    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('archive', type=Path, help='retained dist/cli-release.tar')
    parser.add_argument('--archive-sha256', required=True, help='independently approved SHA-256 of the archive')
    parser.add_argument('--tag', required=True, help='exact approved version tag (already exists in both repos)')
    parser.add_argument('--source', required=True, help='full canonical commit SHA')
    parser.add_argument('--legacy-source', required=True, help='full generated legacy commit SHA')
    parser.add_argument('--mode', required=True, choices=('stable', 'prerelease'), help='approved release policy')
    parser.add_argument('--apply', action='store_true', help='enable writes after separate authorization; default is read-only')
    parser.add_argument('--files', action='store_true', help='include the existing Buildkite bktec Files registry')
    parser.add_argument('--oci', action='append', choices=EXISTING_REPOSITORIES, default=[], help='include this exact legacy OCI route; repeatable')
    parser.add_argument('--oci-layout', type=Path, help='retained two-platform OCI image layout')
    parser.add_argument('--oci-index-sha256', help='independently approved OCI index digest (sha256:...)')
    for service in ('github', 'files', 'oci', 'linux-packages'):
        parser.add_argument(f'--loopback-{service}', type=int, metavar='PORT', help=f'local {service} fixture port')
    args = parser.parse_args()
    try:
        validate_ref(args.tag, args.source, args.legacy_source, mode=args.mode)
        github, stable = select_destinations(args)
        with open_stage(args.archive, args.archive_sha256, args.tag, args.source) as stage:
            reconcile(stage, github, stable_destinations=stable, apply=args.apply)
    except (OSError, ValueError, tarfile.TarError) as error:
        parser.exit(1, f'{error}\n')


if __name__ == '__main__':
    main()
