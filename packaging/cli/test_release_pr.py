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
            real_run = subprocess.run

            def record(args, **kwargs):
                if args[:3] == ['buildkite-agent', 'meta-data', 'set']:
                    metadata[args[3]] = args[4]
                else:
                    return real_run(args, **kwargs)

            with patch.object(subprocess, 'run', side_effect=record):
                pr.prepare(work)
                self.assertIn(f'/compare/main...{pr.branch}?expand=1', output.getvalue())
                self.assertEqual(git(origin, 'rev-parse', 'refs/heads/main'), base)
                self.assertEqual(git(origin, 'rev-parse', f'refs/heads/{pr.branch}'), prepared)
                self.assertEqual(git(origin, 'tag', '--list'), '')

                pr_response = {
                    'merged_at': None,
                    'base': {'ref': 'main'},
                    'head': {'ref': pr.branch, 'sha': prepared, 'repo': {'full_name': 'buildkite/test-engine-client'}},
                }
                with patch.object(release_pr, 'metadata', side_effect=metadata.__getitem__), patch.object(
                    release_pr, 'urlopen', side_effect=lambda *args, **kwargs: io.StringIO(json.dumps([pr_response])),
                ):
                    with self.assertRaisesRegex(ValueError, 'expected the prepared release branch'):
                        pr.merged_commit(work)

                    # GitHub squash merges can replace the topic commit SHA.
                    git(work, 'checkout', 'main')
                    git(work, 'cherry-pick', prepared)
                    git(work, 'commit', '--amend', '-m', 'Reviewed legacy sync (#42)')
                    merged = git(work, 'rev-parse', 'HEAD')
                    pr_response.update(merged_at='2026-11-25T00:00:00Z', merge_commit_sha=merged)
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
