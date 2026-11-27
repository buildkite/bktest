from contextlib import redirect_stdout
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

    def test_retry_keeps_published_tags_and_pushes_only_missing_ones(self):
        repo = 'docker.io/buildkite/test-engine-client'
        tag = 'v3.7.4'
        # The first attempt pushed arm64 from a build that differs from this retry's.
        registry = {'latest': ('sha256:old', {}),
                    f'{tag}-arm64': ('sha256:arm', {'config': {'digest': 'first-arm-image'}})}
        images = {'amd64': 'retry-amd-image', 'arm64': 'retry-arm-image'}

        def docker(*command, **kwargs):
            if command[0] == 'push':
                registry[command[1].rsplit(':', 1)[1]] = ('sha256:amd', {'config': {'digest': 'retry-amd-image'}})
            elif command[:3] == ('buildx', 'imagetools', 'create'):
                registry[tag] = ('sha256:index', {'manifests': [
                    {'platform': {'os': 'linux', 'architecture': ref.rsplit('-', 1)[1]},
                     'digest': registry[ref][0]} for ref in (f'{tag}-amd64', f'{tag}-arm64')]})

        def read(_, ref, *args, **kwargs):
            return registry.get(ref, (None, None))

        with patch('publish_oci.manifest', side_effect=read), patch('publish_oci.docker', side_effect=docker) as calls:
            self.assertEqual(versions(repo, tag, 'token', images),
                             ('sha256:old', {'amd64': 'sha256:amd', 'arm64': 'sha256:arm'}))
            pushed = [call.args[1] for call in calls.call_args_list if call.args[0] == 'push']
            self.assertEqual(pushed, [f'{repo}:{tag}-amd64'])

            # A registry that already has the complete version is left alone.
            calls.reset_mock()
            versions(repo, tag, 'token', images)
            calls.assert_not_called()

    def test_containerd_store_push_indexes_the_platform_manifest(self):
        repo = 'docker.io/buildkite/test-engine-client'
        tag = 'v3.7.4'
        registry = {'latest': ('sha256:old', {})}
        # With the containerd image store, the local image ID is the pushed one-platform index.
        images = {arch: f'sha256:{arch}-index' for arch in ('amd64', 'arm64')}

        def docker(*command, **kwargs):
            if command[0] == 'push':
                arch = command[1].rsplit('-', 1)[1]
                registry[f'{tag}-{arch}'] = (f'sha256:{arch}-index', {'manifests': [
                    {'platform': {'os': 'linux', 'architecture': arch}, 'digest': f'sha256:{arch}-manifest'}]})
            elif command[:3] == ('buildx', 'imagetools', 'create'):
                registry[tag] = ('sha256:index', {'manifests': [
                    {'platform': {'os': 'linux', 'architecture': arch}, 'digest': source.split('@')[1]}
                    for arch, source in zip(('amd64', 'arm64'), command[5:])]})

        def read(_, ref, *args, **kwargs):
            return registry.get(ref, (None, None))

        with patch('publish_oci.manifest', side_effect=read), patch('publish_oci.docker', side_effect=docker), \
                redirect_stdout(io.StringIO()):
            self.assertEqual(versions(repo, tag, 'token', images)[1],
                             {'amd64': 'sha256:amd64-manifest', 'arm64': 'sha256:arm64-manifest'})
