#!/usr/bin/env python3
"""Prepare or upload one approved stable DEB/RPM asset from a retained archive."""

import argparse
import gzip
import hashlib
from pathlib import Path
import re
import subprocess
import tempfile
import time
from urllib.request import Request, urlopen
from xml.etree import ElementTree

from publish import open_stage, validate_ref
from stage import native_path


def read(url):
    with urlopen(Request(url, headers={'Cache-Control': 'no-cache'}), timeout=60) as response:
        return response.read()


def catalog(registry, fmt, arch, scratch):
    if fmt == 'deb':
        base = f'{registry}/any'
        index = scratch / 'InRelease'
        index.write_bytes(read(f'{base}/dists/any/InRelease'))
        signed = subprocess.check_output(['gpgv', '--keyring', scratch / 'key.gpg', '--output', '-', index], text=True)
        path = f'main/binary-{arch}/Packages.gz'
        checksum, = re.findall(rf'^ ([0-9a-f]{{64}}) +\d+ {re.escape(path)}$',
                               signed.split('\nSHA256:\n')[1], re.MULTILINE)
        data = read(f'{base}/dists/any/{path}')
        if hashlib.sha256(data).hexdigest() != checksum:
            raise ValueError('DEB index differs from signed SHA-256')
        for paragraph in gzip.decompress(data).decode().split('\n\n'):
            fields = dict(re.findall(r'^([\w-]+): (.+)$', paragraph, re.MULTILINE))
            if fields.get('Architecture') == arch:
                yield fields['Package'], fields['Version'], f'{base}/{fields["Filename"]}', fields['SHA256']
        return

    rpm_arch = {'amd64': 'x86_64', 'arm64': 'aarch64'}[arch]
    base = f'{registry}/rpm_any/rpm_any/{rpm_arch}'
    index, sig = scratch / 'repomd.xml', scratch / 'repomd.xml.asc'
    index.write_bytes(read(f'{base}/repodata/repomd.xml'))
    sig.write_bytes(read(f'{base}/repodata/repomd.xml.asc'))
    subprocess.run(['gpgv', '--keyring', scratch / 'key.gpg', sig, index], check=True)
    primary = ElementTree.parse(index).find('./{*}data[@type="primary"]')
    data = read(f'{base}/{primary.find("{*}location").attrib["href"]}')
    if hashlib.sha256(data).hexdigest() != primary.findtext('{*}checksum[@type="sha256"]'):
        raise ValueError('RPM index differs from signed SHA-256')
    for entry in ElementTree.fromstring(gzip.decompress(data)).findall('{*}package'):
        if entry.findtext('{*}arch') == rpm_arch:
            yield (entry.findtext('{*}name'), entry.find('{*}version').attrib['ver'],
                   f'{base}/{entry.find("{*}location").attrib["href"]}',
                   entry.findtext('{*}checksum[@type="sha256"]'))


def indexed(registry, fmt, arch, scratch, package, version, expected):
    found = [(url, sha) for name, ver, url, sha in catalog(registry, fmt, arch, scratch)
             if (name, ver) == (package, version)]
    if not found:
        return False
    if len(found) != 1:
        raise ValueError(f'{package} {version}: ambiguous signed package entry')
    url, sha = found[0]
    if sha not in (None, expected) or hashlib.sha256(read(url)).hexdigest() != expected:
        raise ValueError(f'{package} {version}: published package differs from the retained archive')
    return True


def upload(asset, fmt, registry):
    token = subprocess.check_output(['buildkite-agent', 'oidc', 'request-token',
                                     '--audience', registry, '--lifetime', '180'], text=True).strip()
    if not token or any(c.isspace() for c in token):
        raise ValueError('No valid agent OIDC token; no upload attempted.')
    # Read the curl header from stdin, not a process argument or log. Never retry the POST.
    result = subprocess.run(['curl', '-q', '--silent', '--show-error', '--fail', '--output', '/dev/null',
                             '--write-out', '%{http_code}', '--request', 'POST', '--header', '@-',
                             '--form', f'file=@{asset.path}',
                             f'https://api.buildkite.com/v2/packages/organizations/buildkite/'
                             f'registries/test-engine-client-{fmt}/packages'],
                            input=f'Authorization: Bearer {token}\n', text=True, stdout=subprocess.PIPE)
    if result.returncode or not result.stdout.startswith('2'):
        raise ValueError(f'{asset.path.name}: upload not confirmed (HTTP {result.stdout}); verify before retrying')


def publish_pair(stage, fmt, arch):
    registry = f'https://packages.buildkite.com/buildkite/test-engine-client-{fmt}'
    with tempfile.TemporaryDirectory() as temp:
        scratch = Path(temp)
        subprocess.run(['gpg', '--batch', '--dearmor', '--output', scratch / 'key.gpg'],
                       input=read(f'{registry}/gpgkey'), check=True)
        # Wait for bktec in the signed index before exposing the bktest metapackage.
        for package in ('bktec', 'bktest'):
            asset = stage.assets[native_path(package, stage.version, arch, fmt)]
            check = (registry, fmt, arch, scratch, package, stage.version, asset.sha256)
            if not indexed(*check):
                upload(asset, fmt, registry)
                for _ in range(24):
                    time.sleep(10)
                    if indexed(*check):
                        break
                else:
                    raise ValueError(f'{asset.path.name} not indexed yet; verify upload before retrying')
            print(f'Verified {asset.path.name} sha256:{asset.sha256}', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('--archive-sha256', required=True)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--package', choices=('bktec', 'bktest'), default='bktec')
    parser.add_argument('--format', required=True, choices=('deb', 'rpm'))
    parser.add_argument('--arch', required=True, choices=('amd64', 'arm64'))
    parser.add_argument('--confirmed-missing', action='store_true',
                        help='make one write after approval of signed-index exposure and exact absence')
    args = parser.parse_args()
    validate_ref(args.tag, args.source, mode='stable')
    fmt = args.format
    with open_stage(args.archive, args.archive_sha256, args.tag, args.source) as stage:
        asset = stage.assets[native_path(args.package, stage.version, args.arch, fmt)]
        print(f'{asset.path.name} sha256:{asset.sha256}', flush=True)
        if args.confirmed_missing:
            upload(asset, fmt, f'https://packages.buildkite.com/buildkite/test-engine-client-{fmt}')
            print('Read back exact served bytes and the signed index before another write; do not blindly retry.')


if __name__ == '__main__':
    main()
