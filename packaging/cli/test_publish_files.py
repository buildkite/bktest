from dataclasses import dataclass, replace
from email import policy
from email.parser import BytesParser
from functools import partial
from hashlib import sha256
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import tempfile
from threading import Thread
import unittest
from unittest.mock import patch

from publish_files import FilesDestination


API = "/v2/packages/organizations/buildkite/registries/bktec/packages"
DOWNLOAD = "/buildkite/bktec/files/"
VERSION = "3.8.7"
BINARIES = (
    "bktec-darwin-amd64-3.8.7.bin", "bktec-darwin-arm64-3.8.7.bin",
    "bktec-linux-amd64-3.8.7.bin", "bktec-linux-arm64-3.8.7.bin",
    "bktec-windows-amd64-3.8.7.exe", "bktec-windows-arm64-3.8.7.exe",
)
CHECKSUM = "bktec-checksums-3.8.7.txt"


@dataclass(frozen=True)
class Asset:
    path: Path
    sha256: str


@dataclass(frozen=True)
class Stage:
    version: str
    source: str
    archive_sha256: str
    assets: dict[str, Asset]


class FilesAPI(BaseHTTPRequestHandler):
    def __init__(self, state, *args):
        self.state = state
        super().__init__(*args)

    def log_message(self, *args):
        pass

    def reply(self, status, body=b"", *, link=None):
        self.send_response(status)
        if link:
            self.send_header("Link", link)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        state = self.state
        if self.headers.get("Authorization") != "Bearer mock-read":
            return self.reply(403)
        if self.path.startswith(DOWNLOAD):
            name = self.path.removeprefix(DOWNLOAD)
            if name not in state.remote:
                return self.reply(404)
            return self.reply(200, state.downloads.get(name, state.remote[name]))
        if self.path == API + "?per_page=100":
            if state.json_next is not None:
                return self.reply(200, json.dumps({"items": [], "links": {"next": state.json_next}}).encode())
            return self.reply(200, b'{"items": []}', link=f'<{API}?page=2>; rel="next"')
        if self.path != API + "?page=2":
            return self.reply(404)
        if state.list_failure:
            return self.reply(state.list_failure)
        rows = [{"name": "bktec-linux-amd64", "version": "3.7.0"}]
        rows.extend({"name": name.split("-3.8.7.")[0], "version": VERSION,
                     "digests": {"sha256": sha256(data).hexdigest()}}
                    for name, data in state.remote.items())
        return self.reply(200, json.dumps({"items": rows}).encode())

    def do_POST(self):
        state = self.state
        if self.path != API or self.headers.get("Authorization") != "Bearer mock-write":
            return self.reply(403)
        message = BytesParser(policy=policy.default).parsebytes(
            f'Content-Type: {self.headers["Content-Type"]}\r\n\r\n'.encode()
            + self.rfile.read(int(self.headers["Content-Length"]))
        )
        part, = message.iter_parts()
        if part.get_param("name", header="Content-Disposition") != "file":
            return self.reply(400)
        name = part.get_filename()
        state.posts.append(name)
        if name not in (*BINARIES, CHECKSUM) or name in state.remote:
            return self.reply(409)
        state.remote[name] = part.get_payload(decode=True)
        receipt = {"organization": {"slug": "buildkite"}, "registry": {"slug": "bktec"},
                   "name": name.split(f"-{VERSION}.")[0],
                   "digests": {"sha256": sha256(state.remote[name]).hexdigest()}}
        receipt.update(state.receipt_fields)
        if state.bad_receipt == name:
            receipt["digests"]["sha256"] = "0" * 64
        return self.reply(503 if state.fail_after_create == name else 200, json.dumps(receipt).encode())


class FilesDestinationTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name) / 'staging, "approved"'
        self.remote = {}
        self.downloads = {}
        self.posts = []
        self.fail_after_create = None
        self.bad_receipt = None
        self.receipt_fields = {}
        self.list_failure = None
        self.json_next = None
        server = ThreadingHTTPServer(("127.0.0.1", 0), partial(FilesAPI, self))
        self.addCleanup(server.server_close)
        thread = Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.shutdown)
        origin = f"http://127.0.0.1:{server.server_port}"
        self.destination = FilesDestination("mock-read", "mock-write", api_origin=origin,
                                            download_origin=origin, verified_create_only=True)
        assets = {}
        self.contents = {name: b"\x00\xff" + bytes([i]) for i, name in enumerate(BINARIES)}
        for name, (os, arch) in zip(BINARIES, (
            ("darwin", "amd64"), ("darwin", "arm64"), ("linux", "amd64"),
            ("linux", "arm64"), ("windows", "amd64"), ("windows", "arm64"),
        ), strict=True):
            data = self.contents[name]
            exe = ".exe" if os == "windows" else ""
            for key in (f"files/{name}", f"github/bktest/bktest_3.8.7_{os}_{arch}{exe}"):
                assets[key] = self.asset(key, data)
        self.contents[CHECKSUM] = "".join(
            f"{sha256(data).hexdigest()}  {name}\n" for name, data in self.contents.items()
        ).encode()
        assets[f"files/{CHECKSUM}"] = self.asset(f"files/{CHECKSUM}", self.contents[CHECKSUM])
        self.stage = Stage(VERSION, "a" * 40, "b" * 64, assets)

    def asset(self, name, data):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        return Asset(path, sha256(data).hexdigest())

    def test_resume_after_ambiguous_create_and_verify_every_download(self):
        self.enterContext(patch.dict(os.environ, http_proxy="http://127.0.0.1:1", no_proxy="", NO_PROXY=""))
        self.remote[BINARIES[1]] = self.contents[BINARIES[1]]
        self.fail_after_create = BINARIES[2]
        missing = self.destination.preflight(self.stage)
        self.assertEqual(missing, (BINARIES[0], *BINARIES[2:], CHECKSUM))
        self.assertEqual(self.posts, [])
        with self.assertRaisesRegex(OSError, "HTTP 503"):
            self.destination.publish_missing(self.stage, missing)
        self.assertEqual(self.posts, [BINARIES[0], BINARIES[2]])
        self.assertEqual(self.destination.preflight(self.stage), (*BINARIES[3:], CHECKSUM))
        self.destination.publish_missing(self.stage, missing)  # even a stale plan must not re-POST
        self.assertEqual(self.posts, [BINARIES[0], *BINARIES[2:], CHECKSUM])
        self.assertEqual(self.remote, self.contents)
        self.assertEqual(self.destination.preflight(self.stage), ())
        self.downloads[BINARIES[-1]] = b"incorrect served bytes"
        with self.assertRaisesRegex(ValueError, "download hash differs"):
            self.destination.preflight(self.stage)

    def test_json_pagination_checks_next_page_and_refuses_unsafe_links(self):
        self.remote[BINARIES[-1]] = self.contents[BINARIES[-1]]
        self.json_next = "?page=2"
        self.assertNotIn(BINARIES[-1], self.destination.preflight(self.stage))
        for link in ("?per_page=100", "https://other.invalid" + API, "/different?page=2"):
            with self.subTest(link=link), self.assertRaisesRegex(ValueError, "unsafe legacy Files inventory pagination"):
                self.json_next = link
                self.destination.preflight(self.stage)
        self.assertEqual(self.posts, [])

    def test_wrong_create_receipt_stops_before_next_upload(self):
        self.bad_receipt = BINARIES[0]
        with self.assertRaisesRegex(ValueError, "unverifiable Files upload receipt"):
            self.destination.publish_missing(self.stage, self.destination.preflight(self.stage))
        self.assertEqual(self.posts, [BINARIES[0]])
        self.assertNotIn(BINARIES[0], self.destination.preflight(self.stage))

    def test_wrong_receipt_name_or_version_stops_before_next_upload(self):
        for fields in ({"name": "elsewhere"}, {"version": "3.8.6"}):
            with self.subTest(fields=fields):
                self.remote.clear()
                self.posts.clear()
                self.receipt_fields = fields
                with self.assertRaisesRegex(ValueError, "unverifiable Files upload receipt"):
                    self.destination.publish_missing(self.stage, self.destination.preflight(self.stage))
                self.assertEqual(self.posts, [BINARIES[0]])

    def test_receipt_without_version_requires_matching_download_before_next_upload(self):
        self.downloads[BINARIES[0]] = b"wrong served bytes"
        with self.assertRaisesRegex(ValueError, "download hash differs"):
            self.destination.publish_missing(self.stage, self.destination.preflight(self.stage))
        self.assertEqual(self.posts, [BINARIES[0]])

    def test_conflicting_or_unavailable_inventory_is_not_missing(self):
        missing = self.destination.preflight(self.stage)
        self.remote[BINARIES[-1]] = b"conflicting bytes"
        with self.assertRaisesRegex(ValueError, "hash differs"):
            self.destination.publish_missing(self.stage, missing)
        self.remote.clear()
        self.remote["bktec-extra-3.8.7.txt"] = b"unexpected"
        with self.assertRaisesRegex(ValueError, "unexpected or duplicate"):
            self.destination.preflight(self.stage)
        self.list_failure = 404
        with self.assertRaisesRegex(OSError, "HTTP 404"):
            self.destination.preflight(self.stage)
        self.assertEqual(self.posts, [])

    def test_stage_validation_and_explicit_write_gate(self):
        with self.assertRaisesRegex(ValueError, "verified create-only"):
            FilesDestination("mock-read", "mock-write")
        with self.assertRaisesRegex(ValueError, "writes are restricted to loopback"):
            FilesDestination("mock-read", "mock-write", verified_create_only=True)
        with self.assertRaisesRegex(ValueError, "stable CLI version"):
            self.destination.preflight(replace(self.stage, version="3.8.7-rc.1"))
        with self.assertRaisesRegex(ValueError, "unapproved"):
            self.destination.publish_missing(self.stage, ("bktest-linux-amd64-3.8.7.bin",))
        key = "github/bktest/bktest_3.8.7_windows_arm64.exe"
        assets = self.stage.assets | {key: replace(self.stage.assets[key], sha256="0" * 64)}
        with self.assertRaisesRegex(ValueError, "does not match the canonical"):
            self.destination.preflight(replace(self.stage, assets=assets))
        checksum = self.contents[CHECKSUM].replace(b"bktec-windows-arm64", b"bktest-windows-arm64")
        self.stage.assets[f"files/{CHECKSUM}"] = self.asset(f"files/{CHECKSUM}", checksum)
        with self.assertRaisesRegex(ValueError, "checksum entries differ"):
            self.destination.preflight(self.stage)
        self.assertEqual(self.posts, [])
