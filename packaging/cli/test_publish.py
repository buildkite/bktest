import base64
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, HTTPServer
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
from threading import Thread
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

import publish
from publish_github import GitHubRelease
from stage import digest, stage


CANONICAL, LEGACY, ANNOTATED = '1' * 40, '2' * 40, '3' * 40
REPOS = {'bktest': CANONICAL, 'test-engine-client': LEGACY}
TOKEN = 'loopback-fixture-only'


class ReleaseApi(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, data, status=200, headers=None):
        if not isinstance(data, bytes):
            data = json.dumps(data).encode()
        self.send_response(status)
        self.send_header('Content-Length', str(len(data)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(data)

    def dispatch(self):
        url = urlsplit(self.path)
        query = parse_qs(url.query)
        fixture = self.server.fixture
        if url.path.startswith('/download/'):
            if self.headers.get('Authorization'):
                return self.reply({}, 401)
            return self.reply(fixture.downloads[int(url.path.split('/')[-1])])
        if self.headers.get('Authorization') != f'Bearer {TOKEN}':
            return self.reply({}, 401)
        _, prefix, owner, repo, path = url.path.split('/', 4)
        if (prefix, owner) != ('repos', 'buildkite') or repo not in REPOS:
            return self.reply({}, 404)
        release_id = 10 if repo == 'bktest' else 20
        release = fixture.releases.get(repo)
        assets = fixture.remote[repo]
        if self.command == 'GET':
            fixture.reads.append((repo, path))
            if path == 'git/ref/tags/' + fixture.tag:
                sha = fixture.targets.get(repo)
                if sha is None:
                    return self.reply({}, 404)
                obj = {'type': 'tag', 'sha': ANNOTATED} if repo == 'bktest' else {'type': 'commit', 'sha': sha}
                return self.reply({'ref': 'refs/tags/' + fixture.tag, 'object': obj})
            if path == 'git/tags/' + ANNOTATED and repo == 'bktest':
                return self.reply({'object': {'type': 'commit', 'sha': fixture.targets[repo]}})
            if path == 'contents/.bktest-source' and repo == 'test-engine-client' and query == {'ref': [LEGACY]}:
                return self.reply(dict(type='file', encoding='base64', content=base64.b64encode(fixture.projection).decode()))
            if path == 'releases':
                if query['page'] == ['1']:
                    return self.reply([{'tag_name': 'test-collector-js/v1.2.3'}], headers={'Link': '<page2>; rel="next"'})
                return self.reply([release] if release else [])
            if path == f'releases/{release_id}/assets' and release:
                return self.reply(list(assets.values()))
            if path.startswith('releases/assets/') and self.headers.get('Accept') == 'application/octet-stream':
                asset_id = int(path.split('/')[-1])
                if any(item['id'] == asset_id for item in assets.values()):
                    if asset_id % 2:
                        return self.reply(b'', 302, {'Location': f'{fixture.origin}/download/{asset_id}'})
                    return self.reply(fixture.downloads[asset_id])
            return self.reply({}, 404)

        raw = self.rfile.read(int(self.headers['Content-Length']))
        if self.command == 'POST' and path == 'releases':
            payload = json.loads(raw)
            fixture.writes.append((repo, 'create', payload))
            if (release or payload['tag_name'] != fixture.tag or payload['target_commitish'] != REPOS[repo] or
                    payload['draft'] is not True or payload['make_latest'] != 'false'):
                return self.reply({}, 422)
            release = dict(payload, id=release_id, upload_url=(
                f'{fixture.origin}/repos/buildkite/{repo}/releases/{release_id}/assets' + '{?name,label}'))
            if repo == fixture.bad_upload_url:
                release['upload_url'] = fixture.origin + '/wrong{?name,label}'
            fixture.releases[repo] = release
            return self.reply(release, 201)
        if self.command == 'POST' and path == f'releases/{release_id}/assets' and release:
            name = query['name'][0]
            fixture.writes.append((repo, 'upload', name))
            if not release['draft'] or name in assets or self.headers.get('Content-Type') != 'application/octet-stream':
                return self.reply({}, 422)
            if fixture.fail_upload == (repo, name):
                if fixture.leave_starter:
                    asset_id = 100 + len(fixture.downloads)
                    fixture.downloads[asset_id] = b''
                    assets[name] = dict(name=name, id=asset_id, size=0, state='starter')
                return self.reply({}, 502)
            asset_id = 100 + len(fixture.downloads)
            fixture.downloads[asset_id] = raw
            assets[name] = dict(name=name, id=asset_id, size=len(raw), state='uploaded')
            return self.reply(assets[name], 201)
        if self.command == 'PATCH' and path == f'releases/{release_id}' and release:
            payload = json.loads(raw)
            fixture.writes.append((repo, 'publish', payload))
            if payload != {'draft': False, 'make_latest': 'false'} or not release['draft']:
                return self.reply({}, 422)
            release.update(payload, immutable=True)
            return self.reply(release)
        return self.reply({}, 404)

    do_GET = do_POST = do_PATCH = dispatch


class PublicationTest(unittest.TestCase):
    version = '3.999999.99'
    tag = 'v' + version

    @classmethod
    def setUpClass(cls):
        tmp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(tmp.cleanup)
        cls.archive, cls.directory = cls.make_archive(Path(tmp.name), cls.version)

    @staticmethod
    def make_archive(root, version):
        payloads = root / 'payloads'
        payloads.mkdir()
        for system in ('darwin', 'linux', 'windows'):
            for arch in ('amd64', 'arm64'):
                ext = '.exe' if system == 'windows' else ''
                (payloads / f'bktest_{version}_{system}_{arch}{ext}').write_text(f'{system}-{arch}')

        def package(command, *, check):
            config = json.loads(command[command.index('--config') + 1].read_text())
            target = command[command.index('--target') + 1]
            target.write_text(f'{config["arch"]} {target.suffix} package fixture')

        directory = root / 'stage'
        with patch('stage.subprocess.run', side_effect=package):
            stage(version, CANONICAL, payloads, directory)
        archive = root / 'release.tar'
        with tarfile.open(archive, 'w') as tar:
            tar.add(directory, arcname='.')
        return archive, directory

    def setUp(self):
        self.releases = {}
        self.remote = {repo: {} for repo in REPOS}
        self.downloads = {}
        self.reads, self.writes = [], []
        self.targets = dict(REPOS)
        self.projection = f'repository github.com/buildkite/bktest\ncommit {CANONICAL}\n'.encode()
        self.fail_upload = None
        self.leave_starter = False
        self.bad_upload_url = None
        server = HTTPServer(('127.0.0.1', 0), ReleaseApi)
        server.fixture = self
        self.origin = f'http://127.0.0.1:{server.server_port}'
        thread = Thread(target=server.serve_forever, kwargs={'poll_interval': 0.01}, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(thread.join)
        self.addCleanup(server.shutdown)

    def run_driver(self, *, apply=False):
        with publish.open_stage(self.archive, digest(self.archive), self.tag, CANONICAL) as release, redirect_stdout(io.StringIO()):
            destinations = tuple(GitHubRelease(repo, LEGACY, TOKEN, api=self.origin, uploads=self.origin) for repo in REPOS)
            publish.reconcile(release, destinations, apply=apply)

    def test_resume_draft_and_verify_exact_inventory_in_both_repositories(self):
        self.enterContext(patch.dict(os.environ, http_proxy='http://127.0.0.1:1', no_proxy='', NO_PROXY=''))
        self.run_driver()
        self.assertEqual(self.writes, [])
        self.fail_upload = ('test-engine-client', f'bktec_{self.version}_linux_arm64.rpm')
        with self.assertRaisesRegex(ValueError, 'HTTP 502'):
            self.run_driver(apply=True)
        self.assertFalse(self.releases['bktest']['draft'])
        self.assertTrue(self.releases['test-engine-client']['draft'])
        before = dict(self.remote['test-engine-client'])
        self.assertIn('manifest.json', before)
        self.assertEqual(len(self.remote['bktest']), 8)
        self.assertNotIn(self.fail_upload[1], before)
        self.fail_upload = None
        self.writes.clear()
        self.run_driver(apply=True)
        self.assertFalse(self.releases['test-engine-client']['draft'])
        for repo, count in [('bktest', 8), ('test-engine-client', 12)]:
            expected = {p.name: p.read_bytes() for p in (self.directory / 'github' / repo).iterdir()}
            expected['manifest.json'] = (self.directory / 'manifest.json').read_bytes()
            self.assertEqual(len(expected), count)
            self.assertEqual({name: self.downloads[item['id']] for name, item in self.remote[repo].items()}, expected)
            self.assertFalse(self.releases[repo]['prerelease'])
        self.assertEqual({repo for repo, _, _ in self.writes}, {'test-engine-client'})
        self.assertEqual({name for _, action, name in self.writes if action == 'upload'},
                         self.remote['test-engine-client'].keys() - before.keys())
        self.assertEqual({name: self.remote['test-engine-client'][name] for name in before}, before)
        self.writes.clear()
        self.run_driver(apply=True)
        self.assertEqual(self.writes, [])
        self.assertNotIn('releases/latest', [path for _, path in self.reads])

    def test_empty_starter_on_owned_draft_requires_separate_cleanup(self):
        repo, name = self.fail_upload = ('test-engine-client', f'bktec_{self.version}_windows_arm64.exe')
        self.leave_starter = True
        with self.assertRaisesRegex(ValueError, 'HTTP 502'):
            self.run_driver(apply=True)
        self.assertTrue(self.releases[repo]['draft'])
        placeholder = self.remote[repo][name]
        self.fail_upload = None
        self.writes.clear()
        with self.assertRaisesRegex(ValueError, rf'empty starter asset {name} \(asset ID {placeholder["id"]}\)'):
            self.run_driver(apply=True)
        self.assertEqual(self.writes, [])
        self.assertEqual(self.remote[repo][name], placeholder)

        # Simulate separately authorized cleanup, outside the driver.
        del self.remote[repo][name]
        self.run_driver(apply=True)
        self.assertEqual([(r, action) for r, action, _ in self.writes], [(repo, 'upload'), (repo, 'publish')])
        self.assertEqual(self.writes[0][2], name)

    def test_bad_draft_upload_url_stops_before_upload_and_on_cross_repo_preflight(self):
        self.bad_upload_url = 'test-engine-client'
        with self.assertRaisesRegex(ValueError, 'test-engine-client: unexpected release upload URL'):
            self.run_driver(apply=True)
        self.assertEqual([action for repo, action, _ in self.writes if repo == self.bad_upload_url], ['create'])
        self.releases.pop('bktest')
        self.remote['bktest'].clear()
        self.writes.clear()
        for apply in (False, True):
            with self.subTest(apply=apply):
                with self.assertRaisesRegex(ValueError, 'test-engine-client: unexpected release upload URL'):
                    self.run_driver(apply=apply)
                self.assertEqual(self.writes, [])

    def test_wrong_legacy_tag_or_projection_blocks_both_repositories_before_writes(self):
        for target, projection, message in [
            (None, self.projection, 'HTTP 404'),
            (CANONICAL, self.projection, 'peeled .* does not match'),
            (LEGACY, b'unrelated', 'projection does not name'),
        ]:
            self.targets['test-engine-client'], self.projection = target, projection
            with self.subTest(target=target), self.assertRaisesRegex(ValueError, message):
                self.run_driver(apply=True)
            self.assertEqual(self.writes, [])

    def test_existing_conflict_or_unowned_release_blocks_other_repository(self):
        self.run_driver(apply=True)
        # Missing canonical assets must not get repaired when legacy checks fail.
        self.releases.pop('bktest')
        self.remote['bktest'].clear()
        legacy = self.releases['test-engine-client']
        old_body, legacy['body'] = legacy['body'], 'unowned draft'
        legacy['draft'] = True
        self.writes.clear()
        with self.assertRaisesRegex(ValueError, 'reservation differ'):
            self.run_driver(apply=True)
        legacy.update(body=old_body, draft=False)
        asset = self.remote['test-engine-client']['manifest.json']
        self.downloads[asset['id']] = b'!' * asset['size']  # Same size, differing downloaded bytes.
        with self.assertRaisesRegex(ValueError, 'differing asset hash'):
            self.run_driver(apply=True)
        self.assertEqual(self.writes, [])

    def test_incomplete_published_release_blocks_writes_even_if_mutable(self):
        self.run_driver(apply=True)
        self.writes.clear()
        canonical, legacy = REPOS
        for repo, other in ((canonical, legacy), (legacy, canonical)):
            with self.subTest(repo=repo):
                self.releases[repo].pop('immutable')
                if repo == legacy:
                    self.releases[repo]['immutable'] = False
                other_release = self.releases.pop(other)
                other_assets, self.remote[other] = self.remote[other], {}
                manifest = self.remote[repo].pop('manifest.json')
                try:
                    with self.assertRaisesRegex(ValueError, f'{repo}: published release is incomplete'):
                        self.run_driver(apply=True)
                    self.assertEqual(self.writes, [])
                finally:
                    self.remote[repo]['manifest.json'] = manifest
                    self.releases[other] = other_release
                    self.remote[other] = other_assets
                    self.writes.clear()

    def test_bad_archive_pin_source_or_missing_member(self):
        for pin, source, message in [
            ('0' * 64, CANONICAL, 'approved digest'), (digest(self.archive), LEGACY, 'stage manifest differs'),
        ]:
            with self.subTest(source=source), self.assertRaisesRegex(ValueError, message):
                with publish.open_stage(self.archive, pin, self.tag, source):
                    self.fail('unverified stage opened')
        archive = self.archive.with_name('incomplete.tar')
        with tarfile.open(archive, 'w') as tar:
            tar.add(self.directory / 'manifest.json', arcname='manifest.json')
        with self.assertRaisesRegex(ValueError, 'incomplete stage archive inventory'):
            with publish.open_stage(archive, digest(archive), self.tag, CANONICAL):
                self.fail('incomplete stage opened')

    def test_prerelease_mode_guard_and_github_only_publication(self):
        with self.assertRaisesRegex(ValueError, 'release mode does not match'):
            publish.validate_ref(self.tag, CANONICAL, LEGACY, mode='prerelease')
        self.tag += '-rc.1'
        with self.assertRaisesRegex(ValueError, 'release mode does not match'):
            publish.validate_ref(self.tag, CANONICAL, LEGACY, mode='stable')
        with tempfile.TemporaryDirectory() as temp:
            self.archive, _ = self.make_archive(Path(temp), self.tag[1:])
            with publish.open_stage(self.archive, digest(self.archive), self.tag, CANONICAL) as prerelease:
                with self.assertRaisesRegex(ValueError, 'cannot use stable registry destinations'):
                    publish.reconcile(prerelease, (), stable_destinations=(None,), apply=True)
            self.run_driver(apply=True)
        self.assertEqual(set(self.releases), set(REPOS))
        self.assertTrue(all(release['prerelease'] and not release['draft'] for release in self.releases.values()))
        self.assertNotIn('releases/latest', [path for _, path in self.reads])


class PublicationCLITest(unittest.TestCase):
    def test_refuses_unsupported_registry_runs_before_opening_archive_or_network(self):
        args = [sys.executable, '-B', str(Path(publish.__file__)), 'unused.tar', '--archive-sha256', '0' * 64,
                '--source', CANONICAL, '--legacy-source', LEGACY]
        stable = ['--mode', 'stable', '--tag', 'v3.1.2']
        for flags, message in (
            (stable + ['--files', '--apply'], 'real registry writes are disabled'),
            (stable + ['--oci', 'docker.io/buildkite/test-engine-client', '--oci-layout', '.',
                       '--oci-index-sha256', 'sha256:' + '0' * 64, '--apply'], 'real registry writes are disabled'),
            (['--mode', 'prerelease', '--tag', 'v3.1.2-rc.1', '--files'], 'prereleases cannot use stable registry destinations'),
            (stable + ['--loopback-linux-packages', '1234'], 'require --loopback-github'),
            (stable + ['--files', '--loopback-github', '1234'], 'requires its matching port'),
        ):
            with self.subTest(flags=flags):
                result = subprocess.run(args + flags, env={}, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 1, result)
                self.assertIn(message, result.stderr)


if __name__ == '__main__':
    unittest.main()
