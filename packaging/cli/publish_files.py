"""Versioned legacy Files adapter for the documented Buildkite Packages REST API.

The shared driver verifies the archive and source pin. Files has no remote
source marker: the approved stage links the exact downloadable bytes to it.
Requires curl 7.84+ (Link response header via --write-out).
"""

from __future__ import annotations

import hashlib
import json
import re
import subprocess
from typing import TYPE_CHECKING
from urllib.parse import urljoin, urlsplit

from stage import NUMBER, TARGETS, digest

if TYPE_CHECKING:
    from publish import Asset, Stage


class FilesDestination:
    name = "buildkite/bktec"

    def __init__(self, read_token: str, write_token: str | None = None, *,
                 api_origin: str = "https://api.buildkite.com",
                 download_origin: str = "https://packages.buildkite.com",
                 verified_create_only: bool = False):
        if write_token is not None:
            if not verified_create_only:
                raise ValueError("Files writes require verified create-only and channel behavior")
            if not all(re.fullmatch(r"http://127\.0\.0\.1:[0-9]+/?", origin)
                       for origin in (api_origin, download_origin)):
                raise ValueError("Files writes are restricted to loopback rehearsal")
        self.read_token = read_token
        self.write_token = write_token
        self.url = api_origin.rstrip("/") + "/v2/packages/organizations/buildkite/registries/bktec/packages"
        self.download_url = download_origin.rstrip("/") + "/buildkite/bktec/files/"

    def preflight(self, stage: Stage) -> tuple[str, ...]:
        return self._missing(stage.version, self._inventory(stage))

    def publish_missing(self, stage: Stage, missing: tuple[str, ...]) -> None:
        assets = self._inventory(stage)
        if len(set(missing)) != len(missing) or not set(missing) <= assets.keys():
            raise ValueError("unapproved legacy Files names")
        if not missing:
            return
        if self.write_token is None:
            raise ValueError("Files destination is read-only")

        # Recheck since the driver may have published to other destinations since preflight.
        still_missing = set(self._missing(stage.version, assets))
        if not still_missing <= set(missing):
            raise ValueError("legacy Files inventory lost an object after preflight")
        for name, asset in assets.items():
            if name in still_missing:
                # Same multipart POST as publish-to-packages; no replace or channel API.
                # An ambiguous failure stops this run. Resume by reading before any retry.
                path = str(asset.path).replace("\\", "\\\\").replace('"', '\\"')
                self._request(self.url, self.write_token, "--form", f'file=@"{path}"')

    def _inventory(self, stage: Stage) -> dict[str, Asset]:
        if not re.fullmatch(rf"3\.{NUMBER}\.{NUMBER}", stage.version):
            raise ValueError("legacy Files requires a stable CLI version")
        assets = {}
        for os, arch in TARGETS:
            exe = ".exe" if os == "windows" else ""
            name = f"bktec-{os}-{arch}-{stage.version}{exe or '.bin'}"
            asset = stage.assets.get(f"files/{name}")
            canonical = stage.assets.get(f"github/bktest/bktest_{stage.version}_{os}_{arch}{exe}")
            if asset is None or canonical is None or asset.sha256 != canonical.sha256:
                raise ValueError(f"legacy Files does not match the canonical staged payload: {name}")
            assets[name] = asset
        checksum = f"bktec-checksums-{stage.version}.txt"
        expected_lines = "".join(f"{assets[name].sha256}  {name}\n" for name in sorted(assets))
        if {name for name in stage.assets if name.startswith("files/")} != {
            f"files/{name}" for name in (*assets, checksum)
        }:
            raise ValueError("unexpected legacy Files staging inventory")
        assets[checksum] = stage.assets[f"files/{checksum}"]
        for name, asset in assets.items():
            if asset.path.name != name or digest(asset.path) != asset.sha256:
                raise ValueError(f"legacy Files staged filename or hash differs: {name}")
        if assets[checksum].path.read_bytes() != expected_lines.encode():
            raise ValueError("legacy Files checksum entries differ from the six staged binaries")
        return assets

    def _missing(self, version: str, assets: dict[str, Asset]) -> tuple[str, ...]:
        # Files registry entries use the basename without the version and extension.
        basenames = {name.rsplit(f"-{version}.", 1)[0]: name for name in assets}
        found = set()
        url = self.url + "?per_page=100"
        visited = set()
        while url:
            if url in visited or urlsplit(url)._replace(query="").geturl() != self.url:
                raise ValueError("unsafe legacy Files inventory pagination")
            visited.add(url)
            body, link = self._request(url, self.read_token)
            try:
                page = json.loads(body)
                rows = page["items"]
                if not isinstance(rows, list):
                    raise TypeError("items must be an array")
                links = page.get("links", {})
                if not isinstance(links, dict):
                    raise TypeError("links must be an object")
                next_page = links.get("next")
                if next_page is not None and not isinstance(next_page, str):
                    raise TypeError("links.next must be a string")
                for row in rows:
                    if not isinstance(row["version"], str):
                        raise TypeError("version must be a string")
                    if row["version"] != version:
                        continue
                    name = basenames.get(row["name"])
                    if name is None or name in found:
                        raise ValueError("unexpected or duplicate object in legacy Files version")
                    if row["digests"]["sha256"] != assets[name].sha256:
                        raise ValueError(f"legacy Files hash differs: {name}")
                    found.add(name)
            except (KeyError, TypeError, json.JSONDecodeError) as error:
                raise ValueError("invalid legacy Files inventory response") from error
            if not next_page and (header_next := re.search(r'<([^>]+)>;\s*rel="next"', link)):
                next_page = header_next[1]
            url = urljoin(url, next_page) if next_page else None
        # An API basename does not prove the extension or the bytes served at
        # the old download URL. Verify the exact versioned filenames too.
        for name in assets:
            if name in found:
                content, _ = self._request(self.download_url + name, self.read_token,
                                           "--location", "--proto-redir", "=https")
                if hashlib.sha256(content).hexdigest() != assets[name].sha256:
                    raise ValueError(f"legacy Files download hash differs: {name}")
        return tuple(name for name in assets if name not in found)

    def _request(self, url: str, token: str, *options: str) -> tuple[bytes, str]:
        # Pass the token via stdin, not argv. Only downloads follow redirects;
        # curl does not forward Authorization to another host. Never retry a POST.
        result = subprocess.run(
            ["curl", "-q", "--silent", "--max-time", "120", "--proto", "=http,https", "--header", "@-",
             "--write-out", "\n%{http_code}\n%header{link}", *options, url],
            input=f"Authorization: Bearer {token}\n".encode(), capture_output=True,
        )
        if result.returncode:
            raise OSError(f"legacy Files request failed (curl exit {result.returncode})")
        body, status, link = result.stdout.rsplit(b"\n", 2)
        if status != b"200":
            raise OSError(f"legacy Files request failed (HTTP {status.decode()})")
        return body, link.decode()
