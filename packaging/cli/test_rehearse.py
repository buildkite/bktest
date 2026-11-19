import json
from pathlib import Path
import tempfile
import unittest

from rehearse import rehearse
from stage import digest


class RehearsalTest(unittest.TestCase):
    source = "1" * 40

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        self.stage = root / "stage"
        self.destination = root / "destination"
        self.destination.mkdir()
        self.channel = self.destination / "stable.txt"
        self.channel.write_text("v3.5.0\n")

    def stage_fixture(self, version):
        files = [
            f"github/bktest/bktest_{version}_windows_arm64.exe",
            f"github/test-engine-client/bktec_{version}_windows_arm64.exe",
            f"files/bktec-windows-arm64-{version}.exe",
            "image/linux_amd64/Dockerfile",
            "packages/deb/bktec_amd64.deb",
            "tap/Formula/bktec.rb",
        ]
        for name in files:
            path = self.stage / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(name.encode())
        (self.stage / "manifest.json").write_text(json.dumps({
            "version": version,
            "source": self.source,
            "sha256": {name: digest(self.stage / name) for name in files},
        }))
        return files

    def test_prerelease_leaves_existing_channel_and_other_destinations_untouched(self):
        version = "3.6.0-rc.2"
        files = self.stage_fixture(version)
        tag = f"v{version}"
        with self.assertRaisesRegex(ValueError, "cannot advance stable"):
            rehearse(self.stage, self.destination, tag, self.source, True, True)
        self.assertEqual(sorted(self.destination.iterdir()), [self.channel])

        rehearse(self.stage, self.destination, tag, self.source, True, False)
        root = self.destination / tag
        copied = {p.relative_to(root).as_posix() for p in root.rglob("*") if p.is_file()}
        self.assertEqual(copied, {"manifest.json", *files[:2]})
        self.assertEqual(self.channel.read_text(), "v3.5.0\n")
        # Files names are part of the staged versioned inventory, not a channel.
        self.assertTrue((self.stage / files[2]).is_file())

    def test_refuses_wrong_ref_source_or_missing_approval_before_writes(self):
        self.stage_fixture("3.6.0")
        for tag, source, approved in [
            ("", self.source, True), ("test-collector-ruby/v3.6.0", self.source, True),
            ("bktec-history/v3.6.0", self.source, True), ("v3.6.1", self.source, True),
            ("v3.6.0", "2" * 40, True), ("v3.6.0", self.source, False),
        ]:
            with self.subTest(tag=tag, source=source, approved=approved), self.assertRaises(ValueError):
                rehearse(self.stage, self.destination, tag, source, approved, False)
        self.assertEqual(sorted(self.destination.iterdir()), [self.channel])

    def test_resumes_only_missing_files_and_rejects_conflicts(self):
        files = self.stage_fixture("3.6.0")
        tag = "v3.6.0"
        root = self.destination / tag
        first = root / files[0]
        first.parent.mkdir(parents=True)
        first.write_bytes((self.stage / files[0]).read_bytes())
        with self.assertRaisesRegex(ValueError, "without a reservation"):
            rehearse(self.stage, self.destination, tag, self.source, True, False)
        (root / "manifest.json").write_bytes((self.stage / "manifest.json").read_bytes())
        inode = first.stat().st_ino
        rehearse(self.stage, self.destination, tag, self.source, True, False)
        self.assertEqual(first.stat().st_ino, inode)
        self.assertEqual(self.channel.read_text(), "v3.5.0\n")
        self.assertEqual(digest(root / files[2]), digest(self.stage / files[2]))

        first.write_bytes(b"conflict")
        missing = root / files[1]
        missing.unlink()
        with self.assertRaisesRegex(ValueError, "different hash"):
            rehearse(self.stage, self.destination, tag, self.source, True, True)
        self.assertEqual(first.read_bytes(), b"conflict")
        self.assertFalse(missing.exists())
        self.assertEqual(self.channel.read_text(), "v3.5.0\n")

        first.write_bytes((self.stage / files[0]).read_bytes())
        rehearse(self.stage, self.destination, tag, self.source, True, True)
        self.assertEqual(digest(missing), digest(self.stage / files[1]))
        self.assertEqual(self.channel.read_text(), "v3.6.0\n")
