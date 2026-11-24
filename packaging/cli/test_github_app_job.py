import base64
import io
import json
import os
import unittest
from unittest.mock import patch

import github_app_job


class GitHubAppJobTest(unittest.TestCase):
    def test_legacy_write_request_is_scoped_and_publisher_does_not_inherit_key(self):
        env = {
            'BUILDKITE_STEP_KEY': 'cli-sync-legacy',
            'CLI_GITHUB_APP_ID': '17',
            'CLI_GITHUB_INSTALLATION_ID': '42',
            'CLI_GITHUB_PRIVATE_KEY_B64': base64.b64encode(b'fixture-key').decode(),
        }

        def sign(command, *, input):
            self.assertEqual(command[-1].read_bytes(), b'fixture-key')
            claims = input.split(b'.')[1]
            self.assertEqual(json.loads(base64.urlsafe_b64decode(claims + b'==='))['iss'], '17')
            return b'fixture-signature'

        def mint(request, *, timeout):
            self.assertEqual(request.full_url, 'https://api.github.com/app/installations/42/access_tokens')
            self.assertEqual(request.method, 'POST')
            self.assertEqual(json.loads(request.data), {
                'repositories': ['test-engine-client'], 'permissions': {'contents': 'write'},
            })
            self.assertTrue(request.headers['Authorization'].startswith('Bearer ey'))
            return io.BytesIO(b'{"token":"fixture-installation-token"}')

        def exec_writer(_, argv):
            self.assertEqual(argv[2:], ['packaging/cli/sync_legacy.py', '--apply'])
            self.assertEqual(dict(os.environ), {
                'BUILDKITE_STEP_KEY': 'cli-sync-legacy', 'CLI_LEGACY_TOKEN': 'fixture-installation-token',
            })

        with patch.dict(os.environ, env, clear=True), \
                patch('github_app_job.subprocess.check_output', side_effect=sign), \
                patch('github_app_job.build_opener') as opener, \
                patch('github_app_job.os.execv', side_effect=exec_writer) as execute:
            opener.return_value.open.side_effect = mint
            github_app_job.main()
            execute.assert_called_once()
