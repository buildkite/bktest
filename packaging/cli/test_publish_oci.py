import io
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from publish_oci import ARCHES, MANIFEST, PACKAGES_BACKEND, current, digest, encode, layout, upload


class OCITest(unittest.TestCase):
    def test_layout_preserves_platform_manifests(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            archives, expected = [], {}
            for arch in ARCHES:
                archive = root / f'{arch}.tar'
                archives.append(archive)
                with tarfile.open(archive, 'w') as tar:
                    def add(name, data):
                        member = tarfile.TarInfo(name)
                        member.size = len(data)
                        tar.addfile(member, io.BytesIO(data))

                    def blob(data, media_type):
                        sha = digest(data)
                        add('blobs/sha256/' + sha[7:], data)
                        return {'digest': sha, 'size': len(data), 'mediaType': media_type}

                    config = blob(encode({'os': 'linux', 'architecture': arch,
                                          'config': {'Entrypoint': ['/usr/local/bin/bktec']}}),
                                  'application/vnd.oci.image.config.v1+json')
                    manifest = blob(encode({'schemaVersion': 2, 'config': config, 'layers': []}), MANIFEST)
                    add('index.json', encode({'manifests': [manifest]}))
                    expected[f'v3.7.4-{arch}'] = manifest['digest']
            refs = layout(root / 'oci', archives, 'v3.7.4')
            self.assertEqual({k: refs[k] for k in expected}, expected)
            self.assertEqual(digest((root / 'oci/blobs/sha256' / refs['v3.7.4'][7:]).read_bytes()), refs['v3.7.4'])
            with self.assertRaisesRegex(ValueError, 'platform'):
                layout(root / 'swapped', reversed(archives), 'v3.7.4')

    def test_empty_packages_404_requires_matching_control_and_catalog(self):
        base = 'https://registry.invalid/v2/image'
        control = ('v3.5.0', digest(b'known manifest'))
        catalog = {'name': PACKAGES_BACKEND, 'tags': ['v3.5.0', 'v3.6.0', 'v3.6.1']}
        with patch('publish_oci.http', side_effect=[
                (404, b'', None), (200, b'known manifest', None), (200, encode(catalog), None),
                (200, encode(catalog | {'tags': ['v3.6.0', 'v3.6.1']}), None),
                (200, encode(catalog | {'tags': []}), None)]):
            self.assertIsNone(current(base, 'fixture', 'v3.7.4', 'packages', control))
        with patch('publish_oci.http', side_effect=[(404, b'', None), (200, b'wrong manifest', None)]):
            with self.assertRaisesRegex(ValueError, 'control'):
                current(base, 'fixture', 'v3.7.4', 'packages', control)
        with patch('publish_oci.http', side_effect=[
                (404, b'', None), (200, b'known manifest', None),
                (200, encode(catalog | {'name': 'another registry'}), None)]):
            with self.assertRaisesRegex(ValueError, 'inconclusive'):
                current(base, 'fixture', 'v3.7.4', 'packages', control)

    def test_versioned_upload_skips_exact_matches_and_does_not_move_latest(self):
        tag = 'v3.7.4'
        desired = {f'{tag}-amd64': digest(b'A'), f'{tag}-arm64': digest(b'B'), tag: digest(b'AB')}
        existing = {f'{tag}-amd64': desired[f'{tag}-amd64'], 'latest': digest(b'old')}

        def copy(*args):
            self.assertEqual(args[:4], ('skopeo', 'copy', '--all', '--preserve-digests'))
            ref = args[-1].rsplit(':', 1)[1]
            self.assertNotIn(ref, existing)
            existing[ref] = desired[ref]

        with tempfile.TemporaryDirectory() as temp, patch.dict(os.environ, {
                'CLI_DOCKERHUB_USER': 'fixture', 'CLI_DOCKERHUB_TOKEN': 'fixture'}), \
                patch('publish_oci.read_auth', return_value=('fixture', 'fixture')), \
                patch('publish_oci.current', side_effect=lambda _, __, ref, *args: existing.get(ref)), \
                patch('publish_oci.run', side_effect=copy) as write:
            upload('dockerhub', Path(temp) / 'oci', desired, tag, latest=False, control=None)
            self.assertEqual(write.call_count, 2)
            self.assertEqual(existing, desired | {'latest': digest(b'old')})
