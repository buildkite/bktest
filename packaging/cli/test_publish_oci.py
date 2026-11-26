import io
import os
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

from publish_oci import login, manifest, versions


class OCITest(unittest.TestCase):
    def test_ecr_reads_use_the_push_credential(self):
        repo = 'public.ecr.aws/buildkite/test-engine-client'

        def read(request, **kwargs):
            if request.get_header('Authorization') != 'Bearer QVdTOmZpeHR1cmU=':  # base64 of AWS:fixture
                raise HTTPError(request.full_url, 429, 'anonymous rate limit', None, io.BytesIO())
            self.assertEqual(request.full_url, 'https://public.ecr.aws/v2/buildkite/test-engine-client/manifests/latest')
            return io.BytesIO(b'{"manifests": []}')

        with patch.dict(os.environ, {f'ECR_{key}': 'fixture' for key in (
                'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN')}), \
                patch('publish_oci.output', return_value='fixture'), \
                patch('publish_oci.docker') as docker, \
                patch('publish_oci.build_opener', return_value=SimpleNamespace(open=read)):
            self.assertEqual(manifest(repo, 'latest', login(repo))[1], {'manifests': []})
            self.assertEqual(docker.call_args.kwargs['input'], 'fixture\n')

    def test_conflicting_arch_tag_stops_before_any_push(self):
        repo = 'docker.io/buildkite/test-engine-client'
        tag = 'v3.7.4'
        existing = {'latest': ('old', {}), f'{tag}-arm64': ('arm', {'config': {'digest': 'old-image'}})}

        with patch('publish_oci.manifest', side_effect=lambda _, ref, *args, **kw: existing.get(ref, (None, None))), \
                patch('publish_oci.docker') as docker:
            with self.assertRaisesRegex(ValueError, 'different image contents'):
                versions(repo, tag, 'token', {'amd64': 'amd-image', 'arm64': 'arm-image'})
            docker.assert_not_called()
