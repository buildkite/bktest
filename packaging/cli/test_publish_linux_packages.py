"""Run against a locally staged stable-shaped fixture; no external HTTP."""

from dataclasses import dataclass, replace
from email import policy
from email.parser import BytesParser
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import tarfile
import tempfile
import threading
import unittest
from urllib.parse import parse_qs, urlsplit

from publish_linux_packages import LinuxPackagesDestination


@dataclass(frozen=True)
class Asset:
    path: Path
    sha256: str


@dataclass(frozen=True)
class Stage:
    version: str
    source: str
    archive_sha256: str
    assets: dict


def digest(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


@unittest.skipUnless(os.environ.get("BKTEST_PACKAGE_STAGE"), "requires the retained local stage .tar")
class AdapterTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        archive = Path(os.environ["BKTEST_PACKAGE_STAGE"]).resolve()
        if digest(archive) != os.environ["BKTEST_PACKAGE_STAGE_SHA256"]:
            raise ValueError("the retained archive digest differs from the approved fixture digest")
        extracted = tempfile.TemporaryDirectory(dir=archive.parent)
        cls.addClassCleanup(extracted.cleanup)
        root = Path(extracted.name)
        with tarfile.open(archive) as stage_tar:
            stage_tar.extractall(root, filter="data")
        manifest = json.loads((root / "manifest.json").read_text())
        if manifest["source"] != os.environ["BKTEST_PACKAGE_SOURCE"]:
            raise ValueError("the fixture source differs from the approved source")
        assets = {name: Asset(root / name, sha) for name, sha in manifest["sha256"].items()}
        for asset in assets.values():
            if digest(asset.path) != asset.sha256:
                raise ValueError(f"fixture hash differs: {asset.path}")
        cls.stage = Stage(manifest["version"], manifest["source"], os.environ["BKTEST_PACKAGE_STAGE_SHA256"], assets)
        cls.root = root
        cls.names = {fmt: [f"bktec_{cls.stage.version}_linux_{arch}.{fmt}" for arch in ("amd64", "arm64")]
                     for fmt in ("deb", "rpm")}

    def setUp(self):
        self.rows = {"deb": [], "rpm": []}
        self.requests = []
        self.drop_response_after = None
        self.paginate = False
        scenario = self

        class Registry(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def respond(self, status, body, link=None):
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                if link:
                    self.send_header("Link", link)
                self.end_headers()
                self.wfile.write(json.dumps(body).encode())

            def registry(self):
                path = urlsplit(self.path).path
                for fmt in ("deb", "rpm"):
                    if (path == f"/v2/packages/organizations/buildkite/registries/test-engine-client-{fmt}/packages"
                            and self.headers.get("Authorization") == f"Bearer fake-{fmt}"):
                        return fmt
                self.respond(404, {})
                return None

            def do_GET(self):
                fmt = self.registry()
                if fmt is None:
                    return
                page = parse_qs(urlsplit(self.path).query)
                scenario.requests.append(("GET", fmt, page))
                if page == {"name": ["bktec"], "per_page": ["100"], "page": ["1"]}:
                    if fmt == "deb" and scenario.paginate:
                        next_url = scenario.url + self.path[:-1] + "2"
                        self.respond(200, {"items": [{"name": "bktec", "version": "3.5.0"}]}, f'<{next_url}>; rel="next"')
                    else:
                        self.respond(200, {"items": scenario.rows[fmt]})
                elif fmt == "deb" and scenario.paginate and page == {"name": ["bktec"], "per_page": ["100"], "page": ["2"]}:
                    self.respond(200, {"items": scenario.rows[fmt]})
                else:
                    self.respond(400, {})

            def do_POST(self):
                fmt = self.registry()
                if fmt is None:
                    return
                body = self.rfile.read(int(self.headers["Content-Length"]))
                content_type = self.headers["Content-Type"]
                message = BytesParser(policy=policy.default).parsebytes(f"Content-Type: {content_type}\r\n\r\n".encode() + body)
                parts = list(message.iter_parts())
                if len(parts) != 1 or parts[0].get_param("name", header="Content-Disposition") != "file":
                    self.respond(400, {})
                    return
                filename = parts[0].get_filename()
                data = parts[0].get_payload(decode=True)
                sha = hashlib.sha256(data).hexdigest()
                scenario.requests.append(("POST", fmt, filename, sha))
                if filename not in scenario.names[fmt] or sha != scenario.stage.assets[f"packages/{fmt}/{filename}"].sha256:
                    self.respond(400, {})
                    return
                if any(row["digests"]["sha256"] == sha for row in scenario.rows[fmt]):
                    self.respond(409, {})
                    return
                row = scenario.row(fmt, sha)
                scenario.rows[fmt].append(row)
                # Model an upload that committed but returned an error to the caller.
                if package_key(fmt, filename) == scenario.drop_response_after:
                    self.respond(503, {})
                    return
                self.respond(200, {**row, "organization": {"slug": "buildkite"}, "registry": {"slug": f"test-engine-client-{fmt}"}})

        server = ThreadingHTTPServer(("127.0.0.1", 0), Registry)
        self.url = f"http://127.0.0.1:{server.server_port}"
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(thread.join)
        self.addCleanup(server.shutdown)
        self.adapter = LinuxPackagesDestination(self.url, lambda audience: {
            "https://packages.buildkite.com/buildkite/test-engine-client-deb": "fake-deb",
            "https://packages.buildkite.com/buildkite/test-engine-client-rpm": "fake-rpm",
        }[audience])

    def row(self, fmt, sha):
        version = self.stage.version if fmt == "deb" else f"{self.stage.version}-1"
        return {"id": sha[:12], "name": "bktec", "version": version, "digests": {"sha256": sha}}

    def test_exact_requests_and_resume_after_ambiguous_upload(self):
        self.paginate = True
        expected = tuple(package_key(fmt, filename) for fmt in ("deb", "rpm") for filename in self.names[fmt])
        self.assertEqual(self.adapter.preflight(self.stage), expected)
        self.assertFalse(any(req[0] == "POST" for req in self.requests))
        self.drop_response_after = expected[1]
        with self.assertRaisesRegex(OSError, "503.*re-preflight"):
            self.adapter.publish_missing(self.stage, expected)
        self.assertEqual(len(self.rows["deb"]), 2)
        self.assertEqual(self.rows["rpm"], [])
        self.assertEqual(self.adapter.preflight(self.stage), expected[2:])
        self.adapter.publish_missing(self.stage, expected[2:])
        self.assertEqual(self.adapter.preflight(self.stage), ())
        self.assertEqual([package_key(req[1], req[2]) for req in self.requests if req[0] == "POST"], list(expected))

    def test_not_overwriting_conflicting_rpm_when_deb_is_missing(self):
        self.rows["rpm"].append(self.row("rpm", "0" * 64))
        with self.assertRaisesRegex(ValueError, "unverifiable bytes at the exact version"):
            self.adapter.preflight(self.stage)
        self.assertFalse(any(req[0] == "POST" for req in self.requests))

    def test_bad_provenance_bytes_or_native_arch_never_reaches_http(self):
        with self.assertRaisesRegex(ValueError, "loopback"):
            LinuxPackagesDestination("https://api.buildkite.com", None)
        for stage, error in [(replace(self.stage, version=self.stage.version + "-rc.1"), "stable"),
                             (replace(self.stage, archive_sha256="unverified"), "provenance")]:
            with self.assertRaisesRegex(ValueError, error):
                self.adapter.preflight(stage)
        with tempfile.TemporaryDirectory(dir=self.root) as temp:
            filename, foreign = self.names["deb"]
            key = package_key("deb", filename)
            wrong = self.stage.assets[package_key("deb", foreign)]
            path = Path(temp) / filename
            path.hardlink_to(wrong.path)
            assets = dict(self.stage.assets)
            assets[key] = replace(assets[key], path=path)
            with self.assertRaisesRegex(ValueError, "staged bytes changed"):
                self.adapter.preflight(replace(self.stage, assets=assets))
            assets[key] = assets[f"github/test-engine-client/{filename}"] = Asset(path, wrong.sha256)
            with self.assertRaisesRegex(ValueError, "wrong DEB identity"):
                self.adapter.preflight(replace(self.stage, assets=assets))
        self.assertEqual(self.requests, [])


def package_key(fmt, filename):
    return f"packages/{fmt}/{filename}"
