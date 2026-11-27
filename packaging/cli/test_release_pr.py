from contextlib import redirect_stdout
import io
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import release_pr
import sync_legacy
from release_pr import ReleasePR, git


class ReleasePRTest(unittest.TestCase):
    def test_tags_only_the_verified_merge_of_the_prepared_branch(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict('os.environ', {
            'BUILDKITE_BUILD_ID': '078ed665-ad51-4938-b645-71d219e0e740',
            'GH_TOKEN': 'test-token',
        }), redirect_stdout(io.StringIO()) as output:
            root = Path(directory)
            origin = root / 'legacy.git'
            work = root / 'work'
            git(root, 'init', '--bare', '--initial-branch=main', str(origin))
            git(root, 'clone', str(origin), str(work))
            git(work, 'config', 'user.email', 'test@example.com')
            git(work, 'config', 'user.name', 'Release test')
            marker = work / '.bktest-source'
            marker.write_text('old\n')
            git(work, 'add', '.')
            git(work, 'commit', '-m', 'Legacy main')
            git(work, 'push', 'origin', 'main')
            base = git(work, 'rev-parse', 'HEAD')
            git(work, 'checkout', '--detach')
            marker.write_text('approved projection\n')
            git(work, 'commit', '-am', 'Proposed sync')
            prepared = git(work, 'rev-parse', 'HEAD')
            tag = 'v3.7.0-rc.1'
            pr = ReleasePR('test-engine-client', 'main', tag)
            pr.url = str(root / 'legacy')
            metadata = {}
            annotations = {}
            pulls = []
            pr_response = {
                'number': 42, 'state': 'open', 'merged_at': None,
                'base': {'ref': 'main'},
                'head': {'ref': pr.branch, 'sha': prepared, 'repo': {'full_name': 'buildkite/test-engine-client'}},
            }
            real_run = subprocess.run

            def record(args, **kwargs):
                if args[:3] == ['buildkite-agent', 'meta-data', 'set']:
                    metadata[args[3]] = args[4]
                elif args[:2] == ['buildkite-agent', 'annotate']:
                    annotations[args[4]] = args[2]
                else:
                    return real_run(args, **kwargs)

            def github(request, *, timeout):
                if isinstance(request, str):
                    return io.StringIO(json.dumps(pulls))
                self.assertEqual(request.get_method(), 'POST')
                self.assertEqual(request.full_url, 'https://api.github.com/repos/buildkite/test-engine-client/pulls')
                self.assertEqual(request.get_header('Authorization'), 'Bearer test-token')
                data = json.loads(request.data)
                self.assertEqual((data['base'], data['head']), ('main', pr.branch))
                self.assertFalse(pulls, 'retry must not create another PR')
                pulls.append(pr_response)
                return io.StringIO(json.dumps(pr_response))

            with (patch.object(subprocess, 'run', side_effect=record),
                  patch.object(release_pr, 'urlopen', side_effect=github)):
                pr.prepare(work)
                git(work, 'fetch', 'origin')
                pr.prepare(work)
                self.assertIn('/pull/42', output.getvalue())
                self.assertEqual(list(annotations.values()), [
                    f'Review and merge: [test-engine-client #42]({pr.url}/pull/42)',
                ])
                self.assertEqual(len(pulls), 1)
                self.assertEqual(git(origin, 'rev-parse', 'refs/heads/main'), base)
                self.assertEqual(git(origin, 'rev-parse', f'refs/heads/{pr.branch}'), prepared)
                self.assertEqual(git(origin, 'tag', '--list'), '')

                with patch.object(release_pr, 'metadata', side_effect=metadata.__getitem__):
                    with self.assertRaisesRegex(ValueError, 'expected the prepared release branch'):
                        pr.merged_commit(work)

                    # GitHub squash merges can replace the topic commit SHA.
                    git(work, 'checkout', 'main')
                    git(work, 'cherry-pick', prepared)
                    git(work, 'commit', '--amend', '-m', 'Reviewed legacy sync (#42)')
                    merged = git(work, 'rev-parse', 'HEAD')
                    pr_response.update(state='closed', merged_at='2026-11-25T00:00:00Z', merge_commit_sha=merged)
                    with self.assertRaises(subprocess.CalledProcessError):
                        pr.merged_commit(origin)
                    pr_response['head']['sha'] = base
                    with self.assertRaisesRegex(ValueError, 'expected the prepared release branch'):
                        pr.merged_commit(work)
                    pr_response['head']['sha'] = prepared
                    self.assertNotEqual(merged, prepared)
                    self.assertEqual(pr.merged_commit(work), merged)

                projector = SimpleNamespace(committed_projection=lambda path, _: (path / marker.name).read_text())
                with patch.object(sync_legacy, 'LEGACY', str(origin)):
                    with self.assertRaisesRegex(ValueError, 'does not match the approved projection'):
                        sync_legacy.tag_merge(work, projector, 'wrong bytes\n', merged, tag)
                    self.assertEqual(git(origin, 'tag', '--list'), '')
                    git(work, 'push', 'origin', 'main')  # role of the reviewed PR merge
                    sync_legacy.tag_merge(work, projector, 'approved projection\n', merged, tag)
                    sync_legacy.tag_merge(work, projector, 'approved projection\n', merged, tag)
                self.assertEqual(git(origin, 'rev-parse', f'refs/tags/{tag}'), merged)
                self.assertEqual(metadata['cli-legacy-source'], merged)
