"""Exact-version bktec DEB/RPM adapter. HTTP transport is limited to local rehearsal."""

import hashlib
import io
import json
import re
import subprocess
import tarfile
from urllib.error import HTTPError
from urllib.parse import parse_qs, urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener
from uuid import uuid4


REGISTRIES = {fmt: f"test-engine-client-{fmt}" for fmt in ("deb", "rpm")}
ARCHES = {"amd64": "x86_64", "arm64": "aarch64"}
SHA256 = re.compile(r"[0-9a-f]{64}")


def _digest(data):
    return hashlib.file_digest(data, "sha256").hexdigest()


def _native_inventory(path, fmt, arch, version, payload_sha):
    if fmt == "deb":
        fields = subprocess.check_output(["dpkg-deb", "-f", path, "Package", "Version", "Architecture"], text=True)
        if fields.splitlines() != ["Package: bktec", f"Version: {version}", f"Architecture: {arch}"]:
            raise ValueError(f"wrong DEB identity: {path.name}")
        data = subprocess.check_output(["dpkg-deb", "--fsys-tarfile", path])
    else:
        fields = subprocess.check_output([
            "rpm", "--nosignature", "-qp", "--queryformat", "%{NAME} %{EPOCHNUM} %{VERSION} %{RELEASE} %{ARCH}", path,
        ], text=True)
        if fields != f"bktec 0 {version} 1 {ARCHES[arch]}":
            raise ValueError(f"wrong RPM identity: {path.name}")
        with path.open("rb") as package:
            data = subprocess.check_output(["rpm2archive", "-n"], stdin=package)

    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        entries = [entry for entry in archive if not entry.isdir()]
        if sorted(entry.name.removeprefix("./") for entry in entries) != ["usr/bin/bktec", "usr/bin/bktest"]:
            raise ValueError(f"wrong package contents: {path.name}")
        for entry in entries:
            if not entry.isfile() or entry.mode & 0o111 != 0o111 or _digest(archive.extractfile(entry)) != payload_sha:
                raise ValueError(f"{path.name}: {entry.name} is not the staged executable")


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class LinuxPackagesDestination:
    """Implements the shared driver's destination protocol over a loopback fake.

    The driver supplies its already validated Stage and release approval. Live
    HTTP remains disabled until list/digest, create-only and visibility contracts
    are confirmed for these registries.
    """

    name = "Buildkite bktec DEB/RPM"

    def __init__(self, api_base, token_for_audience):
        parsed = urlsplit(api_base)
        if (parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or not parsed.port
                or parsed.username is not None or parsed.path not in ("", "/") or parsed.query or parsed.fragment):
            raise ValueError("DEB/RPM publication transport is restricted to loopback rehearsal")
        self.api_base = api_base.rstrip("/")
        self.token_for_audience = token_for_audience
        self.opener = build_opener(ProxyHandler({}), _NoRedirect())

    def _inventory(self, stage):
        if not re.fullmatch(r"3\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", stage.version):
            raise ValueError("DEB/RPM accepts only exact stable CLI versions")
        if not re.fullmatch(r"[0-9a-f]{40}", stage.source) or not SHA256.fullmatch(stage.archive_sha256):
            raise ValueError("verified stage provenance is required")
        packages = {fmt: {} for fmt in REGISTRIES}
        for fmt, assets in packages.items():
            for arch in ARCHES:
                filename = f"bktec_{stage.version}_linux_{arch}.{fmt}"
                key = f"packages/{fmt}/{filename}"
                asset = stage.assets.get(key)
                legacy = stage.assets.get(f"github/test-engine-client/{filename}")
                payload = stage.assets.get(f"github/bktest/bktest_{stage.version}_linux_{arch}")
                if (asset is None or legacy is None or payload is None or not SHA256.fullmatch(asset.sha256)
                        or asset.sha256 != legacy.sha256 or asset.path.name != filename or asset.path.is_symlink()):
                    raise ValueError(f"missing or inconsistent verified staging entries: {key}")
                with asset.path.open("rb") as package:
                    if _digest(package) != asset.sha256:
                        raise ValueError(f"staged bytes changed: {key}")
                try:
                    _native_inventory(asset.path, fmt, arch, stage.version, payload.sha256)
                except (subprocess.CalledProcessError, tarfile.TarError) as error:
                    raise ValueError(f"cannot inspect staged package: {key}") from error
                assets[key] = asset
        return packages

    def _request(self, fmt, path, *, body=None, content_type=None):
        audience = f"https://packages.buildkite.com/buildkite/{REGISTRIES[fmt]}"
        token = self.token_for_audience(audience)
        if not token:
            raise ValueError(f"missing registry token for {REGISTRIES[fmt]}")
        headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
        if content_type:
            headers["Content-Type"] = content_type
        try:
            with self.opener.open(Request(self.api_base + path, data=body, headers=headers), timeout=30) as response:
                if response.status != 200:
                    raise OSError(f"{REGISTRIES[fmt]} returned unexpected HTTP {response.status}")
                result = json.load(response)
                if not isinstance(result, dict):
                    raise ValueError(f"unexpected response from {REGISTRIES[fmt]}")
                return result, response.headers.get("Link", "")
        except HTTPError as error:
            raise OSError(f"{REGISTRIES[fmt]} returned HTTP {error.code}; stop and re-preflight before retry") from error

    def _path(self, fmt):
        return f"/v2/packages/organizations/buildkite/registries/{REGISTRIES[fmt]}/packages"

    def _missing(self, stage, packages):
        missing = []
        for fmt, assets in packages.items():
            versions = {stage.version} if fmt == "deb" else {stage.version, f"{stage.version}-1"}
            by_digest = {asset.sha256: key for key, asset in assets.items()}
            found = set()
            page = 1
            while True:
                query = urlencode({"name": "bktec", "per_page": 100, "page": page})
                result, link = self._request(fmt, f"{self._path(fmt)}?{query}")
                items = result.get("items")
                if not isinstance(items, list):
                    raise ValueError(f"missing package inventory in {REGISTRIES[fmt]}")
                for item in items:
                    if (not isinstance(item, dict) or item.get("name") != "bktec"
                            or not isinstance(item.get("version"), str) or not item["version"]):
                        raise ValueError(f"unexpected listing in {REGISTRIES[fmt]}")
                    digests = item.get("digests")
                    sha = digests.get("sha256") if isinstance(digests, dict) else None
                    if item["version"] not in versions:
                        if isinstance(sha, str) and sha in by_digest:
                            raise ValueError(f"staged bytes listed under an unexpected version in {REGISTRIES[fmt]}")
                        continue
                    key = by_digest.get(sha) if isinstance(sha, str) else None
                    if key is None or key in found:
                        raise ValueError(f"different, duplicate or unverifiable bytes at the exact version in {REGISTRIES[fmt]}")
                    found.add(key)

                next_links = re.findall(r'<([^>]+)>;\s*rel="next"', link)
                if not next_links:
                    break
                if len(next_links) != 1:
                    raise ValueError("ambiguous package pagination")
                target = urlsplit(next_links[0])
                expected = urlsplit(self.api_base + self._path(fmt))
                params = parse_qs(target.query)
                if ((target.scheme, target.netloc, target.path) != (expected.scheme, expected.netloc, expected.path)
                        or params != {"name": ["bktec"], "per_page": ["100"], "page": [str(page + 1)]}):
                    raise ValueError("unsafe package pagination")
                page += 1
            missing.extend(key for key in assets if key not in found)
        return tuple(missing)

    def preflight(self, stage):
        return self._missing(stage, self._inventory(stage))

    def publish_missing(self, stage, missing):
        packages = self._inventory(stage)
        current = self._missing(stage, packages)
        if len(set(missing)) != len(missing) or not set(current) <= set(missing) <= {
            key for assets in packages.values() for key in assets
        }:
            raise ValueError("package inventory changed; start a new preflight")
        for fmt, assets in packages.items():
            for key, asset in assets.items():
                if key not in current:
                    continue
                payload = asset.path.read_bytes()
                if hashlib.sha256(payload).hexdigest() != asset.sha256:
                    raise ValueError(f"staged bytes changed before upload: {key}")
                boundary = uuid4().hex
                body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{asset.path.name}"\r\n'
                        'Content-Type: application/octet-stream\r\n\r\n').encode() + payload + f"\r\n--{boundary}--\r\n".encode()
                result, _ = self._request(fmt, self._path(fmt), body=body, content_type=f"multipart/form-data; boundary={boundary}")
                if (result.get("name") != "bktec" or result.get("registry", {}).get("slug") != REGISTRIES[fmt]
                        or result.get("organization", {}).get("slug") != "buildkite"
                        or result.get("digests", {}).get("sha256") != asset.sha256):
                    raise ValueError(f"unverifiable upload response for {key}; stop and re-preflight before retry")
