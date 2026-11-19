"""GitHub versioned release destination. No tag, overwrite, or delete endpoints."""

import base64
import hashlib
import json
from pathlib import Path
import re
from urllib.error import HTTPError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class GitHubRelease:
    def __init__(self, repo, legacy_source, token, *, api='https://api.github.com',
                 uploads='https://uploads.github.com'):
        if repo not in ('bktest', 'test-engine-client') or not re.fullmatch(r'[a-f0-9]{40}', legacy_source):
            raise ValueError('expected a known GitHub release destination and full legacy source SHA')
        self.repo = repo
        self.name = f'github/buildkite/{repo}'
        self.legacy_source = legacy_source
        self.token = token
        self.api = api
        self.uploads = uploads
        self.prefix = f'/repos/buildkite/{repo}'
        self.opener = build_opener(NoRedirect())

    def request(self, url, *, accept='application/vnd.github+json', data=None, headers=None, status=200, method=None):
        req = Request(url, data=data, method=method, headers={
            'Accept': accept, 'Authorization': f'Bearer {self.token}',
            'X-GitHub-Api-Version': '2022-11-28', **(headers or {}),
        })
        try:
            response = self.opener.open(req, timeout=60)
        except HTTPError as error:
            # The API may hand off asset downloads to a signed URL. Do not
            # forward API credentials or follow redirects on a write.
            with error:
                location = error.headers.get('Location', '')
                if data is None and accept == 'application/octet-stream' and error.code == 302:
                    if urlsplit(location).scheme == 'https' or (
                        urlsplit(self.api).scheme == 'http' and location.startswith(self.api + '/')
                    ):
                        try:
                            return self.opener.open(Request(location), timeout=60)
                        except HTTPError as redirect_error:
                            with redirect_error:
                                raise ValueError(f'{self.name}: asset download redirect returned HTTP {redirect_error.code}') from None
                raise ValueError(f'{self.name}: {req.get_method()} {urlsplit(url).path} returned HTTP {error.code}') from None
        if response.status != status:
            with response:
                raise ValueError(f'{self.name}: expected HTTP {status}, got {response.status}')
        return response

    def get(self, path):
        with self.request(self.api + self.prefix + path) as response:
            return json.load(response)

    def pages(self, path):
        page = 1
        while True:
            with self.request(f'{self.api}{self.prefix}{path}?per_page=100&page={page}') as response:
                entries = json.load(response)
                next_page = 'rel="next"' in response.headers.get('Link', '')
            yield from entries
            if not next_page:
                return
            page += 1

    def target(self, stage):
        return stage.source if self.repo == 'bktest' else self.legacy_source

    def verify_source(self, stage):
        tag = f'v{stage.version}'
        ref = self.get(f'/git/ref/tags/{tag}')
        if ref['ref'] != f'refs/tags/{tag}':
            raise ValueError(f'{self.name}: API returned the wrong tag ref')
        obj = ref['object']
        seen = set()
        while obj['type'] == 'tag' and re.fullmatch(r'[a-f0-9]{40}', obj['sha']) and obj['sha'] not in seen:
            seen.add(obj['sha'])
            obj = self.get(f"/git/tags/{obj['sha']}")['object']
        if obj['type'] != 'commit' or obj['sha'] != self.target(stage):
            raise ValueError(f'{self.name}: peeled {tag} does not match the approved commit')
        if self.repo == 'test-engine-client':
            marker = self.get(f'/contents/.bktest-source?ref={self.legacy_source}')
            expected = f'repository github.com/buildkite/bktest\ncommit {stage.source}\n'.encode()
            if (marker['type'] != 'file' or marker['encoding'] != 'base64' or
                    base64.b64decode(marker['content']) != expected):
                raise ValueError(f'{self.name}: legacy projection does not name the approved canonical source')

    def body(self, stage):
        return (f'bktest release stage SHA-256: {stage.archive_sha256}\n'
                f'Canonical source: {stage.source}\nLegacy source: {self.legacy_source}\n')

    def verify_release(self, release, stage):
        if (release['tag_name'] != f'v{stage.version}' or type(release['draft']) is not bool or
                release['prerelease'] is not ('-' in stage.version) or release['body'] != self.body(stage)):
            raise ValueError(f'{self.name}: existing release flags or stage reservation differ')
        release_id = release['id']
        if type(release_id) is not int or release_id <= 0:
            raise ValueError(f'{self.name}: invalid release ID')
        return release_id

    def upload_endpoint(self, release, release_id):
        url = f'{self.uploads}{self.prefix}/releases/{release_id}/assets'
        if release.get('upload_url') != url + '{?name,label}':
            raise ValueError(f'{self.name}: unexpected release upload URL for draft {release_id}')
        return url

    def inspect(self, stage):
        self.verify_source(stage)
        expected = {Path(name).name: asset for name, asset in stage.assets.items()
                    if name.startswith(f'github/{self.repo}/')}
        expected['manifest.json'] = stage.assets['manifest.json']
        # The by-tag endpoint only finds published releases. List so a
        # preexisting draft also blocks reuse of this version.
        matches = [item for item in self.pages('/releases') if item['tag_name'] == f'v{stage.version}']
        if not matches:
            return None, expected
        if len(matches) != 1:
            raise ValueError(f'{self.name}: multiple releases for the same tag')
        release = matches[0]
        release_id = self.verify_release(release, stage)
        remaining = dict(expected)
        for asset in self.pages(f'/releases/{release_id}/assets'):
            name = asset['name']
            if name not in remaining:
                raise ValueError(f'{self.name}: unexpected or duplicate asset {name}')
            local = remaining.pop(name)
            asset_id = asset['id']
            if type(asset_id) is not int or asset_id <= 0:
                raise ValueError(f'{self.name}: invalid asset ID for {name}')
            if release['draft'] and asset['state'] == 'starter' and type(asset['size']) is int and asset['size'] == 0:
                raise ValueError(f'{self.name}: v{stage.version} draft {release_id} has empty starter asset {name} '
                                 f'(asset ID {asset_id}); stopped. See publish.py --help for separately authorized cleanup')
            if asset['state'] != 'uploaded' or asset['size'] != local.path.stat().st_size:
                raise ValueError(f'{self.name}: differing or incomplete asset {name}')
            with self.request(f'{self.api}{self.prefix}/releases/assets/{asset_id}',
                              accept='application/octet-stream') as response:
                observed = hashlib.file_digest(response, 'sha256').hexdigest()
            if observed != local.sha256:
                raise ValueError(f'{self.name}: differing asset hash for {name}')
        if remaining:
            if not release['draft']:
                raise ValueError(f'{self.name}: published release is incomplete; only owned drafts can resume')
            self.upload_endpoint(release, release_id)
        return release, remaining

    def preflight(self, stage):
        release, remaining = self.inspect(stage)
        names = set(remaining)
        if release is None or release['draft']:
            names.add(f'release:v{stage.version}')
        return tuple(sorted(names))

    def publish_missing(self, stage, missing):
        release, remaining = self.inspect(stage)
        if release is not None and not release['draft']:
            return  # Already complete; never modify a published release.
        names = remaining.keys() | {f'release:v{stage.version}'}
        if names - set(missing):
            raise ValueError(f'{self.name}: inventory changed after preflight; rerun reconciliation')
        if release is None:
            # Use a draft so repositories with immutable releases can accept
            # all assets before publish, and an interruption keeps it hidden.
            data = json.dumps(dict(tag_name=f'v{stage.version}', target_commitish=self.target(stage),
                                   name=f'v{stage.version}', body=self.body(stage), draft=True,
                                   prerelease='-' in stage.version, make_latest='false')).encode()
            with self.request(self.api + self.prefix + '/releases', data=data,
                              headers={'Content-Type': 'application/json'}, status=201) as response:
                release = json.load(response)
        release_id = self.verify_release(release, stage)
        if not release['draft']:
            raise ValueError(f'{self.name}: release visibility changed unexpectedly')
        url = self.upload_endpoint(release, release_id) if remaining else None
        for name in sorted(remaining, key=lambda name: (name != 'manifest.json', name)):
            asset = remaining[name]
            with asset.path.open('rb') as data, self.request(
                f'{url}?{urlencode({"name": name})}', data=data, status=201,
                headers={'Content-Type': 'application/octet-stream', 'Content-Length': str(asset.path.stat().st_size)},
            ) as response:
                uploaded = json.load(response)
            if (uploaded['name'] != name or uploaded['state'] != 'uploaded' or
                    uploaded['size'] != asset.path.stat().st_size):
                raise ValueError(f'{self.name}: unexpected upload result for {name}')
        draft, pending = self.inspect(stage)
        if pending or draft is None or draft['id'] != release_id or not draft['draft']:
            raise ValueError(f'{self.name}: draft is not ready to publish')
        data = json.dumps(dict(draft=False, make_latest='false')).encode()
        with self.request(f'{self.api}{self.prefix}/releases/{release_id}', method='PATCH', data=data,
                          headers={'Content-Type': 'application/json'}) as response:
            published = json.load(response)
        if self.verify_release(published, stage) != release_id or published['draft']:
            raise ValueError(f'{self.name}: release was not published as expected')
